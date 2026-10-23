package party.openwatch

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.channels.ReceiveChannel
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.addJsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonArray
import kotlinx.serialization.json.putJsonObject
import kotlin.math.abs
import kotlin.random.Random
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNotNull
import kotlin.test.assertTrue

/** Whole parties against a real relay built from relay/ (test mode: relay timers × 0.1). */
internal class IntegrationTest : RelayTest() {
    private val movie = Media(MediaKind.MOVIE, "Heat", durationMs = 600_000, ids = mapOf("imdb" to "tt0113277"))

    /** Finds [movie] 300 ms longer than the reference. */
    private val findsMovie = Resolver { media: Media ->
        if (sameTitle(media, movie)) listOf(Candidate("heat", "heat.mkv", durationMs = media.durationMs + 300)) else emptyList()
    }

    private class Watching(
        val a: WatchParty<String>,
        val b: WatchParty<String>,
        val pa: FakePlayer,
        val pb: FakePlayer,
        val code: String,
    )

    /** Anna creates, Ben joins, Anna shares [movie] at 60 s; returns once both players run. */
    private suspend fun watching(relays: Relays): Watching {
        val a = party(relays, "Anna")
        val b = party(relays, "Ben", findsMovie)
        val code = a.create()
        b.join(code)
        val pa = FakePlayer(clock, movie.durationMs)
        val pb = FakePlayer(clock, movie.durationMs + 300)
        a.setMedia(movie, "heat.mkv", positionMs = 60_000)
        a.attachPlayer(pa, "heat.mkv")
        eventually("Ben's match") { b.state.value.choice != null }
        assertEquals(Match(MatchState.MATCHED, 300), b.state.value.match)
        b.attachPlayer(pb, "heat.mkv")
        eventually("both players running") {
            a.state.value.room?.phase == Phase.PLAYING && !pa.paused && !pb.paused
        }
        return Watching(a, b, pa, pb, code)
    }

    private fun near(x: Long, y: Long, tolerance: Long) = abs(x - y) <= tolerance

    @Test
    fun createAndJoinWithATypedCode() = test {
        val relays = relaysAt(relay().port)
        val a = party(relays, "Anna")
        val b = party(relays, "Ben")
        val aEvents = recordEvents(a)
        val code = a.create()
        assertTrue(Owp.ALPHABET.let { alphabet -> code.length == 6 && code.all { it in alphabet } }, code)
        assertEquals(Connection.CONNECTED, a.state.value.connection)
        assertTrue(a.state.value.isHost)

        b.join(formatCode(code).lowercase())
        assertEquals(Connection.CONNECTED, b.state.value.connection)
        assertEquals(code, b.state.value.room?.code)
        assertFalse(b.state.value.isHost)
        eventually("Anna sees Ben") { a.state.value.room?.participants?.size == 2 }
        val joined = aEvents.filterIsInstance<PartyEvent.ParticipantJoined>().single()
        assertEquals("Ben", joined.participant.name)
        assertEquals("OwpKotlinTest/1.0", joined.participant.client)

        // SDK.md §7.2: once the host ends the room, its join of the same typed code with create makes the room again.
        a.end()
        eventually("the room ended for both") {
            a.state.value.connection == Connection.ENDED && b.state.value.connection == Connection.ENDED
        }
        a.join(formatCode(code).lowercase(), create = true)
        assertEquals(Connection.CONNECTED, a.state.value.connection)
        assertEquals(code, a.state.value.room?.code)
        assertTrue(a.state.value.isHost)
        assertEquals(listOf("Anna"), a.state.value.room?.participants?.map { it.name })
    }

    @Test
    fun setMediaMatchesAndBothPlayersFollow() = test {
        val w = watching(relaysAt(relay().port))
        eventually("both ready") { w.a.state.value.room?.participants?.all { it.player == PlayerState.READY } == true }
        assertEquals(PlayerState.READY, w.a.state.value.player)
        assertEquals(PlayerState.READY, w.b.state.value.player)
        delay(1000)
        val room = assertNotNull(w.a.roomPositionMs())
        assertTrue(near(w.pa.positionMs, room, 300), "Anna at ${w.pa.positionMs}, room at $room")
        assertTrue(near(w.pb.positionMs, room, 300), "Ben at ${w.pb.positionMs}, room at $room")
        assertTrue(room > 60_500, "the room plays from 60 s: $room")
    }

    @Test
    fun pauseSeekAndRateReachTheOtherPlayer() = test {
        val w = watching(relaysAt(relay().port))
        val bEvents = recordEvents(w.b)

        w.a.pause()
        eventually("both paused") { w.pa.paused && w.pb.paused && w.b.state.value.room?.phase == Phase.PAUSED }
        assertTrue(near(w.pa.positionMs, w.pb.positionMs, 600), "Anna ${w.pa.positionMs}, Ben ${w.pb.positionMs}")
        val pause = bEvents.filterIsInstance<PartyEvent.Action>().single { it.kind == "pause" }
        assertEquals(w.a.state.value.peerId, pause.byPeerId)
        assertEquals("Anna", pause.by?.name)
        assertFalse(pause.byMe)

        w.a.seek(120_000)
        eventually("Ben at 120 s") { w.b.state.value.room?.positionMs == 120_000L && w.pb.positionMs == 120_000L }
        assertEquals(120_000L, w.pa.positionMs)

        w.a.setRate(1.5)
        eventually("Ben at rate 1.5") { w.b.state.value.room?.rate == 1.5 && w.pb.rate == 1.5 }

        w.a.play()
        eventually("both playing") { w.b.state.value.room?.phase == Phase.PLAYING && !w.pa.paused && !w.pb.paused }
        delay(1000)
        assertTrue(w.pa.positionMs > 120_000 + 1000, "Anna moved at 1.5×: ${w.pa.positionMs}")
        assertTrue(near(w.pa.positionMs, w.pb.positionMs, 400), "Anna ${w.pa.positionMs}, Ben ${w.pb.positionMs}")
        assertEquals(listOf("pause", "seek", "rate", "play"), bEvents.filterIsInstance<PartyEvent.Action>().map { it.kind })
    }

    @Test
    fun aDroppedConnectionIsResumed() = test {
        val relay = relay()
        val proxy = use(TcpProxy(Ports.take(), relay.port))
        val w = watching(relaysAt(proxy.port))
        val peers = w.a.state.value.peerId to w.b.state.value.peerId
        val aEvents = recordEvents(w.a)

        proxy.cut()
        eventually("reconnecting") { w.a.state.value.connection == Connection.RECONNECTING }
        eventually("resumed") {
            w.a.state.value.connection == Connection.CONNECTED && w.b.state.value.connection == Connection.CONNECTED &&
                w.a.state.value.room?.participants?.all { it.connected } == true
        }
        assertEquals(peers, w.a.state.value.peerId to w.b.state.value.peerId)
        assertEquals(w.code, w.a.state.value.room?.code)
        assertTrue(aEvents.none { it is PartyEvent.Ended || it is PartyEvent.ParticipantLeft })

        // The room paused while nobody was connected; it plays again for both. The new connections' clocks have few
        // samples yet, so a busy machine starts them apart; the rate nudge brings them together.
        w.a.play()
        eventually("both playing") { w.b.state.value.room?.phase == Phase.PLAYING && !w.pa.paused && !w.pb.paused }
        eventually("in step") { near(w.pa.positionMs, w.pb.positionMs, 400) }
    }

    /**
     * SPEC.md §3.1: a relay message of 32768 bytes and more arrives through the app's Ktor client. A relay's rooms stay
     * below that (SPEC.md §13.2), so a scripted relay answers `join` with a `joined` padded past it by a member the SDK
     * doesn't know.
     */
    @Test
    fun aRelayMessageOf32768BytesAndMoreIsAccepted() = test {
        val (code, peerId, token) = Triple("K7MQ2X", "p1", "A".repeat(43))
        val scripted = use(
            ScriptedRelay(Ports.take()) { message ->
                if (message["type"]?.jsonPrimitive?.content != "join") return@ScriptedRelay null
                buildJsonObject {
                    put("type", "joined")
                    put("code", code)
                    putJsonObject("you") {
                        put("peerId", peerId)
                        put("token", token)
                    }
                    putJsonObject("room") {
                        put("rev", 1)
                        put("code", code)
                        put("epoch", 0)
                        put("phase", "idle")
                        put("positionMs", 0)
                        put("atMs", 0)
                        put("rate", 1.0)
                        put("control", "host")
                        put("locked", false)
                        putJsonArray("waitingOn") {}
                        putJsonArray("participants") {
                            addJsonObject {
                                put("peerId", peerId)
                                put("name", "Ben")
                                put("client", "OwpKotlinTest/1.0")
                                put("host", true)
                                put("connected", true)
                                put("player", "idle")
                                put("match", JsonNull)
                                put("offsetMs", 0)
                            }
                        }
                    }
                    put("media", JsonNull)
                    put("serverTime", System.currentTimeMillis())
                    put("padding", "x".repeat(32768))
                }.toString()
            },
        )
        val measured = Measured(KtorTransport(http, scope), scope)
        val party = WatchParty(
            relaysAt(scripted.port),
            "OwpKotlinTest/1.0",
            "Ben",
            Resolver { emptyList<Candidate<String>>() },
            scope,
            clock,
            Random.Default,
            measured,
        )
        party.join(code)
        assertTrue(measured.largest > 32768, "the largest message was ${measured.largest} bytes")
        assertEquals(Connection.CONNECTED, party.state.value.connection)
        assertEquals("K7MQ2X", party.state.value.room?.code)
    }

    /** Records the UTF-8 size of the largest text message its sockets receive. */
    private class Measured(private val inner: Transport, private val scope: CoroutineScope) : Transport {
        @Volatile var largest = 0

        override suspend fun open(url: String): Socket {
            val socket = inner.open(url)
            val incoming = Channel<String>(Channel.UNLIMITED)
            scope.launch {
                try {
                    for (text in socket.incoming) {
                        largest = maxOf(largest, utf8Length(text))
                        incoming.send(text)
                    }
                } finally {
                    incoming.close()
                }
            }
            return object : Socket by socket {
                override val incoming: ReceiveChannel<String> = incoming
            }
        }
    }
}
