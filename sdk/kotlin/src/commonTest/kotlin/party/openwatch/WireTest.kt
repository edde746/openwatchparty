package party.openwatch

import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.respondError
import io.ktor.http.HttpStatusCode
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.cancel
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonObject
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue
import kotlin.test.fail

/**
 * What the SDK sends is valid UTF-8 without repeated member names (SDK.md §7.1). Unpaired surrogates can't travel in
 * the JSON vectors, so they are tested here.
 */
class WireTest {
    private val highAlone = "\uD83D"
    private val lowAlone = "\uDE00"
    private val emoji = "\uD83D\uDE00"

    private fun construct(name: String, client: String) {
        val http = HttpClient(MockEngine { respondError(HttpStatusCode.NotFound) })
        val scope = CoroutineScope(Job())
        try {
            WatchParty(Relays.public(http), client, name, { emptyList<Candidate<String>>() }, scope)
        } finally {
            scope.cancel()
            http.close()
        }
    }

    private fun refused(name: String, client: String) {
        val e = assertFailsWith<PartyException>("name ${name.escaped()}, client ${client.escaped()}") { construct(name, client) }
        assertEquals(PartyException.INVALID_MESSAGE, e.code)
    }

    private fun String.escaped() = map { if (it.code in 0x20..0x7e) "$it" else "\\u" + it.code.toString(16).padStart(4, '0') }.joinToString("")

    @Test
    fun theConstructorRefusesUnpairedSurrogates() {
        for (bad in listOf(highAlone, lowAlone, "Anna$highAlone", "${lowAlone}Anna", lowAlone + highAlone, "$emoji$highAlone")) {
            refused(name = bad, client = "Test/1.0")
            refused(name = "Ben", client = "Test/1.0$bad")
        }
        construct(name = emoji.repeat(32), client = "Test/$emoji")
    }

    private val movie = Media(MediaKind.MOVIE, "Heat", durationMs = 600_000)

    @Test
    fun checkMediaRefusesUnpairedSurrogatesAnywhere() {
        val bad = "x$highAlone"
        val broken = listOf(
            movie.copy(title = bad),
            movie.copy(ids = mapOf("imdb" to bad)),
            movie.copy(ids = mapOf(bad to "1")),
            movie.copy(file = MediaFile(name = bad)),
            Media(MediaKind.EPISODE, "Pilot", 1000, series = Series(bad)),
            Media(MediaKind.EPISODE, "Pilot", 1000, series = Series("Show", ids = mapOf("tvdb" to bad))),
            movie.copy(ext = buildJsonObject { putJsonObject("com.example") { put(bad, 1) } }),
            movie.copy(ext = buildJsonObject { putJsonObject("com.example") { put("key", bad) } }),
            movie.copy(ext = buildJsonObject { put("com.example", JsonArray(listOf(JsonPrimitive(lowAlone)))) }),
        )
        for (media in broken) assertEquals(true, checkMedia(media)?.endsWith("has an unpaired surrogate"), "$media: ${checkMedia(media)}")
        assertNull(checkMedia(movie.copy(title = emoji, ext = buildJsonObject { putJsonObject("com.example") { put(emoji, emoji) } })))
    }

    @Test
    fun checkMediaRefusesNumbersBeyondAnInt() {
        val max = (1L shl 53) - 1
        assertNull(checkMedia(movie.copy(year = -max, season = max)))
        for (media in listOf(movie.copy(season = max + 1), movie.copy(year = -max - 1), movie.copy(file = MediaFile(sizeBytes = Long.MAX_VALUE)))) {
            assertEquals("a number is beyond ±(2^53 − 1)", checkMedia(media), "$media")
        }
    }

    /** SPEC.md §2: ints go out as digits only, also when they came from arithmetic. */
    @Test
    fun intsGoOutWithoutAFractionOrExponent() {
        val max = (1L shl 53) - 1
        val offset = -604_800_000L
        assertEquals("""{"type":"seek","positionMs":9007199254740991}""", Out.seek(max + offset - offset))
        assertEquals("""{"type":"pause","positionMs":1000000}""", Out.pause(maxOf(0L, 1_000_000L / 2 * 2)))
        assertEquals(
            """{"type":"status","epoch":3,"player":"ready","match":{"state":"mismatched","deltaMs":-604799999},"offsetMs":-604800000,"rttMs":20}""",
            Out.status(StatusFields(3, PlayerState.READY, Match(MatchState.MISMATCHED, 1 - 604_800_000L), offset, 40 / 2)),
        )
        val media = Media(
            MediaKind.EPISODE, "Pilot", durationMs = 604_800_000, year = 2008, series = Series("Breaking Bad", year = 2008),
            season = 0, episode = 1, file = MediaFile(sizeBytes = 4_700_000_000),
        )
        assertEquals(
            """{"type":"setMedia","media":{"kind":"episode","title":"Pilot","year":2008,"series":{"title":"Breaking Bad","year":2008},""" +
                """"season":0,"episode":1,"durationMs":604800000,"file":{"sizeBytes":4700000000}},"positionMs":1000}""",
            Out.setMedia(media.toJson(), 1_000),
        )
        assertEquals("""{"type":"ping","id":1}""", Out.ping(1))
    }

    @Test
    fun theEncoderNeverRepeatsAMemberName() {
        // Building an object with a name twice keeps the last value only: the encoder writes maps.
        val twice = buildJsonObject {
            put("a", 1)
            put("a", 2)
        }
        assertEquals("""{"a":2}""", twice.toString())

        // ext stays its own object: names that the descriptor or the message also use don't merge with them.
        val media = Media(
            MediaKind.EPISODE,
            "Pilot",
            durationMs = 1000,
            ids = mapOf("tvdb" to "1"),
            series = Series("Show", ids = mapOf("tvdb" to "2")),
            season = 1,
            episode = 1,
            file = MediaFile(name = "pilot.mkv"),
            ext = buildJsonObject {
                putJsonObject("com.example") {
                    put("type", "x")
                    put("kind", "y")
                    put("title", "z")
                    putJsonObject("ids") { put("tvdb", "3") }
                }
            },
        )
        assertNull(checkMedia(media))
        val messages = listOf(
            Out.create("Ben", "Test/1.0"),
            Out.join("K7MQ2X", "Ben", "Test/1.0", create = false),
            Out.join("K7MQ2X", "Ben", "Test/1.0", create = true),
            Out.resume("K7MQ2X", "p1", "secret"),
            Out.ping(1),
            Out.status(StatusFields(1, PlayerState.READY, Match(MatchState.MATCHED, 0), 0, 20)),
            Out.setMedia(media.toJson(), 0),
            Out.setMedia(parseMedia(media.toJson()).toJson(), 0),
            Out.pause(1000),
            Out.seek(1000),
            Out.rate(1.5),
        )
        messages.forEach(::assertNoRepeatedNames)
    }

    /** SDK.md §9.7: parseMedia applies the receive checks (no sizes) and refuses with `invalid_message`. */
    @Test
    fun parseMediaAppliesTheReceiveChecksAndThrowsInvalidMessage() {
        val big = buildJsonObject { put("com.example", "x".repeat(2000)) }
        val received = JsonObject(movie.toJson() + mapOf("ext" to big, "extra" to JsonPrimitive(1)))
        assertEquals(movie.copy(ext = big), parseMedia(received), "ext over 1024 bytes passes; unknown members are dropped")
        assertEquals("ext is over 1024 bytes", checkMedia(movie.copy(ext = big)), "on send it doesn't")
        val refused = listOf(
            JsonArray(listOf(movie.toJson())),
            JsonObject(movie.toJson() - "durationMs"),
            JsonObject(movie.toJson() + ("year" to JsonPrimitive("1995"))),
            JsonObject(movie.toJson() + ("file" to buildJsonObject { put("name", "../heat.mkv") })),
        )
        for (json in refused) {
            assertEquals(PartyException.INVALID_MESSAGE, assertFailsWith<PartyException>("$json") { parseMedia(json) }.code)
        }
    }

    @Test
    fun theDuplicateCheckFindsDuplicates() {
        assertFailsWith<AssertionError> { assertNoRepeatedNames("""{"type":"setMedia","media":{"ext":{"a.b":1,"a.b":2}}}""") }
        assertFailsWith<AssertionError> { assertNoRepeatedNames("""{"type":"x","\u0074ype":"y"}""") }
        assertNoRepeatedNames("""{"a":{"a":[{"a":1},{"a":2}]}}""")
    }
}

/** Fails when an object anywhere in the JSON [text] has two members of the same name (SPEC.md §4). */
internal fun assertNoRepeatedNames(text: String) {
    var i = 0
    fun skipSpace() {
        while (i < text.length && text[i] in " \t\n\r") i++
    }
    fun expect(c: Char) {
        if (text.getOrNull(i) != c) fail("expected '$c' at $i in $text")
        i++
    }
    fun string(): String {
        expect('"')
        val out = StringBuilder()
        while (text[i] != '"') {
            val c = text[i++]
            if (c != '\\') {
                out.append(c)
                continue
            }
            when (val e = text[i++]) {
                'u' -> {
                    out.append(text.substring(i, i + 4).toInt(16).toChar())
                    i += 4
                }
                'b' -> out.append('\b')
                'f' -> out.append('\u000c')
                'n' -> out.append('\n')
                'r' -> out.append('\r')
                't' -> out.append('\t')
                else -> out.append(e)
            }
        }
        i++
        return out.toString()
    }
    fun value(path: String) {
        skipSpace()
        when (text.getOrNull(i)) {
            '{' -> {
                i++
                val names = mutableSetOf<String>()
                skipSpace()
                if (text[i] == '}') {
                    i++
                    return
                }
                while (true) {
                    skipSpace()
                    val name = string()
                    assertTrue(names.add(name), "member \"$name\" twice in $path of $text")
                    skipSpace()
                    expect(':')
                    value("$path.$name")
                    skipSpace()
                    if (text[i] == ',') i++ else break
                }
                expect('}')
            }
            '[' -> {
                i++
                skipSpace()
                if (text[i] == ']') {
                    i++
                    return
                }
                var n = 0
                while (true) {
                    value("$path[${n++}]")
                    skipSpace()
                    if (text[i] == ',') i++ else break
                }
                expect(']')
            }
            '"' -> string()
            null -> fail("unexpected end of $text")
            else -> while (i < text.length && text[i] !in ",:}] \t\n\r") i++
        }
    }
    value("$")
    skipSpace()
    assertNotNull(text.takeIf { i == text.length }, "trailing text in $text")
}
