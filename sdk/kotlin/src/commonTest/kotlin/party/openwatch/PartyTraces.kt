package party.openwatch

import io.ktor.client.HttpClient
import io.ktor.client.engine.mock.MockEngine
import io.ktor.client.engine.mock.MockEngineConfig
import io.ktor.client.engine.mock.respond
import io.ktor.http.HttpStatusCode
import kotlinx.coroutines.CancellableContinuation
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.CoroutineExceptionHandler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.Delay
import kotlinx.coroutines.DisposableHandle
import kotlinx.coroutines.InternalCoroutinesApi
import kotlinx.coroutines.Runnable
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.channels.ReceiveChannel
import kotlinx.coroutines.launch
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlin.coroutines.CoroutineContext
import kotlin.coroutines.resume
import kotlin.math.floor
import kotlin.math.roundToLong
import kotlin.random.Random
import kotlin.test.fail

/*
 * The driver of conformance/vectors/party.json (SDK.md §17): one trace runs a real WatchParty against a scripted
 * relay, scripted adapters and a scripted resolver, in virtual time. conformance/vectors/README.md (the party section)
 * defines the format; VectorTest runs every trace through [runPartyTrace].
 */

/** Runs one trace and fails with the first step whose outputs differ, or with an end-of-trace failure. */
internal fun runPartyTrace(input: JsonObject, expect: JsonObject) {
    val steps = input["steps"].asArray() ?: fail("no steps")
    val outputs = expect["outputs"].asArray() ?: fail("no outputs")
    if (outputs.size != steps.size) fail("${steps.size} steps but ${outputs.size} outputs")
    val run = TraceRun(input["setup"].asObject() ?: fail("no setup"))
    try {
        run.time.settle()
        for ((i, step) in steps.withIndex()) {
            val s = step.asObject() ?: fail("step $i is not an object")
            run.step(s)
            run.problems.firstOrNull()?.let { fail("step $i $s: $it") }
            val want = unordered(canonical(outputs[i]).asObject() ?: fail("output $i is not an object"))
            val got = unordered(canonical(run.take(want)).asObject()!!)
            if (got != want) fail("step $i $s at local ${run.time.now}:\n  got  $got\n  want $want")
        }
        if (run.draws.left > 0) fail("setup.random holds ${run.draws.left} draw(s) the party never took")
        run.resolving?.let { fail("the resolve of ${it.title} is still unanswered") }
        if (run.probes.isNotEmpty()) fail("the probes of ${run.probes.keys} are still unanswered")
        for ((id, player) in run.players) if (player.held != null) fail("$id still runs a held command")
        val answered = outputs.flatMap { o -> o.asObject()?.get("done").asArray().orEmpty().map { it.asObject()?.get("id").asString() } }
        for (id in run.calls) if (id !in answered) fail("call $id never settled")
    } finally {
        run.close()
    }
}

/**
 * Virtual local time: the dispatcher every coroutine of the trace runs on, with its own timers. [settle] runs
 * everything ready without moving time; [advance] moves time timer by timer, earliest first and equal times in the order
 * they were set, settling after each. Nothing waits for the wall clock.
 */
@OptIn(InternalCoroutinesApi::class)
private class VirtualTime(var now: Long) : CoroutineDispatcher(), Delay {
    private class Timer(val at: Long, val seq: Long, val run: () -> Unit)

    private val ready = ArrayDeque<Runnable>()
    private val timers = mutableListOf<Timer>()
    private var seq = 0L

    override fun isDispatchNeeded(context: CoroutineContext): Boolean = true

    override fun dispatch(context: CoroutineContext, block: Runnable) {
        ready.addLast(block)
    }

    fun schedule(delayMs: Long, run: () -> Unit): () -> Unit {
        val at = if (delayMs <= 0) now else if (delayMs > Long.MAX_VALUE - now) Long.MAX_VALUE else now + delayMs
        val timer = Timer(at, seq++, run)
        timers += timer
        return { timers.remove(timer) }
    }

    override fun scheduleResumeAfterDelay(timeMillis: Long, continuation: CancellableContinuation<Unit>) {
        val cancel = schedule(timeMillis) { continuation.resume(Unit) }
        continuation.invokeOnCancellation { cancel() }
    }

    override fun invokeOnTimeout(timeMillis: Long, block: Runnable, context: CoroutineContext): DisposableHandle {
        val cancel = schedule(timeMillis) { block.run() }
        return DisposableHandle { cancel() }
    }

    private fun next(limit: Long): Timer? = timers.filter { it.at <= limit }.minWithOrNull(compareBy({ it.at }, { it.seq }))

    /** Runs continuations and the timers due now until nothing is ready. */
    fun settle() {
        var budget = 1_000_000
        while (true) {
            if (--budget == 0) fail("the party never settles at local $now")
            val task = ready.removeFirstOrNull()
            if (task != null) {
                task.run()
                continue
            }
            val timer = next(now) ?: return
            timers.remove(timer)
            timer.run()
        }
    }

    fun advance(ms: Long) {
        val end = now + ms
        while (true) {
            val timer = next(end) ?: break
            now = timer.at
            settle()
        }
        now = end
        settle()
    }
}

/** The randomness seam: `nextDouble()` yields `setup.random` in order; every other draw is a failure. */
private class Draws(values: List<Double>, private val problems: MutableList<String>) : Random() {
    private val queue = ArrayDeque(values)
    val left: Int get() = queue.size

    override fun nextDouble(): Double = queue.removeFirstOrNull() ?: run {
        problems += "the party drew more random values than setup.random holds"
        0.0
    }

    override fun nextBits(bitCount: Int): Int {
        problems += "the party drew random bits, not one u through nextDouble() (SDK.md §16.8)"
        return 0
    }
}

/** A running resolve or probe call. */
private class Pending<V>(val title: String, val answer: CompletableDeferred<V> = CompletableDeferred())

/**
 * Everything the party did in response to one step, per channel; a sent message as `{on, msg}` and a failed call's
 * `done` with `re` and `message`, until [TraceRun.take] shapes them as the trace writes them.
 */
private class Outputs {
    val send = mutableListOf<JsonObject>()
    val command = mutableListOf<JsonObject>()
    val event = mutableListOf<JsonElement>()
    val resolver = mutableListOf<JsonObject>()
    val net = mutableListOf<JsonObject>()
    val done = mutableListOf<JsonObject>()

    fun channels(): List<Pair<String, List<JsonElement>>> =
        listOf("send" to send, "command" to command, "event" to event, "resolver" to resolver, "net" to net, "done" to done)
}

/** One trace's world: the party, its fakes and the outputs of the current step. */
private class TraceRun(private val setup: JsonObject) {
    val problems = mutableListOf<String>()
    val time = VirtualTime(setup.long("startMs"))
    val draws = Draws(setup["random"].asArray().orEmpty().map { it.asNumber() ?: fail("random holds $it") }, problems)
    private val job = SupervisorJob()
    private val scope = CoroutineScope(time + job + CoroutineExceptionHandler { _, e -> problems += "uncaught $e" })
    private var out = Outputs()
    private val sockets = mutableListOf<FakeSocket>()
    val players = linkedMapOf<String, FakePlayer>()
    var resolving: Pending<List<Candidate<String>>>? = null
    val probes = linkedMapOf<String, Pending<Long?>>()
    val calls = mutableSetOf<String>()

    /** How the next connection opens when a `nextOpen` step scripted it: per URL, and for any URL under "". */
    private val nextOpen = mutableMapOf<String, String>()

    /** The round trip of pings sent from now on, per URL and for every other URL; null: never answered. */
    private val rtt = mutableMapOf<String, Long?>()
    private var rttAll: Long? = setup.long("rttMs")
    private var nextGet: JsonElement? = null
    private val list = setup["list"] ?: fail("no list")

    private val engine = MockEngine(
        MockEngineConfig().apply {
            dispatcher = time
            addHandler { request -> respond(get(request.url.toString()), HttpStatusCode.OK) }
        },
    )
    private val http = HttpClient(engine)
    private val clock = MonotonicClock { time.now }
    val relays: Relays = when (val base = setup["base"]) {
        null, is JsonNull -> Relays.public(http, clock)
        else -> Relays.at(http, base.asString() ?: fail("base is $base"))
    }
    val party = WatchParty(
        relays = relays,
        client = setup.string("client"),
        name = setup.string("name"),
        resolver = if (setup["resolver"].asBoolean() == true) Resolver { resolve(it) } else null,
        scope = scope,
        clock = clock,
        random = draws,
        transport = { url -> connect(url) },
    )

    init {
        scope.launch(start = CoroutineStart.UNDISPATCHED) { party.events.collect { out.event += project(it) } }
    }

    // -- The relay ----------------------------------------------------------------------------------------------------

    /** A relay connection: pings are answered by the driver, everything else the party sends is an output. */
    private inner class FakeSocket(val url: String) : Socket {
        private val inbox = Channel<String>(Channel.UNLIMITED)
        override val incoming: ReceiveChannel<String> get() = inbox

        /** Open until either side closes it. */
        var open = true
            private set
        private var code: Int? = null

        override suspend fun closeCode(): Int? = code

        override fun send(text: String) {
            val message = Json.parseToJsonElement(text)
            if (message.asObject()?.get("type").asString() == "ping") {
                pong(this, message.asObject()!!["id"]!!)
            } else {
                out.send += obj("on" to url, "msg" to message)
            }
        }

        /** The party closing the connection; only an open one reports it. */
        override fun close(code: Int) {
            if (open) out.net += obj("close" to url, "code" to code)
            open = false
            inbox.close()
        }

        fun deliver(text: String) {
            if (open) inbox.trySend(text)
        }

        /** The relay closing the connection with [code] (null: none). */
        fun drop(code: Int?) {
            this.code = code
            open = false
            inbox.close()
        }
    }

    /** The transport seam: every connection opens at once unless a `nextOpen` step scripted it, for its URL first. */
    private suspend fun connect(url: String): Socket {
        out.net += obj("open" to url)
        return when (nextOpen.remove(if (url in nextOpen) url else "")) {
            "fail" -> throw IllegalStateException("the connection to $url failed")
            "hang" -> awaitCancellation()
            else -> FakeSocket(url).also { sockets += it }
        }
    }

    /** Answers a ping its URL's round trip later, with relay time = send time + floor(rtt / 2) + relayOffsetMs. */
    private fun pong(socket: FakeSocket, id: JsonElement) {
        val ms = (if (socket.url in rtt) rtt[socket.url] else rttAll) ?: return
        val serverTime = time.now + ms / 2 + setup.long("relayOffsetMs")
        time.schedule(ms) { socket.deliver(obj("type" to "pong", "id" to id, "serverTime" to serverTime).toString()) }
    }

    /** The connection opened last that is still open, to [on] when given. */
    private fun socket(on: String?): FakeSocket =
        sockets.lastOrNull { it.open && (on == null || it.url == on) } ?: fail("no open connection${on?.let { " to $it" } ?: ""}")

    /** A GET of the list's source: `setup.list` unless a `nextGet` step scripted this one. */
    private suspend fun get(url: String): String {
        out.net += obj("get" to url)
        val how = nextGet ?: list
        nextGet = null
        return when (how.asString()) {
            "fail" -> throw IllegalStateException("GET $url failed")
            "hang" -> awaitCancellation()
            else -> how.toString()
        }
    }

    // -- The resolver -------------------------------------------------------------------------------------------------

    private suspend fun resolve(media: Media): List<Candidate<String>> {
        if (resolving != null) problems += "a second resolve call while one is pending"
        out.resolver += obj("resolve" to media.title)
        val pending = Pending<List<Candidate<String>>>(media.title)
        resolving = pending
        try {
            return pending.answer.await()
        } catch (e: CancellationException) {
            if (resolving === pending) resolving = null
            out.resolver += obj("resolveCancelled" to media.title)
            throw e
        }
    }

    private suspend fun probe(id: String): Long? {
        if (id in probes) problems += "probe $id called twice at once"
        out.resolver += obj("probe" to id)
        val pending = Pending<Long?>(id)
        probes[id] = pending
        try {
            return pending.answer.await()
        } catch (e: CancellationException) {
            if (probes[id] === pending) probes.remove(id)
            out.resolver += obj("probeCancelled" to id)
            throw e
        }
    }

    private fun candidate(c: JsonElement?): Candidate<String> {
        val o = c.asObject() ?: fail("candidate $c")
        val id = o.string("id")
        return Candidate(
            id = id,
            ref = o["ref"]?.let { it.asString() ?: fail("ref $it") } ?: id,
            durationMs = o.longOrNull("durationMs"),
            sizeBytes = o.longOrNull("sizeBytes"),
            oshash = o["oshash"].asString(),
            name = o["name"].asString(),
            probe = if (o["probe"].asBoolean() == true) suspend { probe(id) } else null,
        )
    }

    // -- The players --------------------------------------------------------------------------------------------------

    /** A command the trace holds until a `finish` step. */
    class Held(val apply: () -> Unit, val done: CompletableDeferred<Unit> = CompletableDeferred())

    /** A scripted adapter. Its position moves at the rate last set while it plays; commands take effect when they finish. */
    inner class FakePlayer(private val id: String) : PlayerAdapter {
        private var position = 0L
        private var since = time.now
        private var rate = 1.0
        override var paused = true
        override var buffering = false
        override var durationMs: Long? = null
        override var firstFrameShown = true
        override var canSetRate = false
        override var seekAccuracyMs = 0L
        var playThrows = false

        /** The next command stays running until a `finish` step. */
        var holdNext = false
        var held: Held? = null

        override val positionMs: Long
            get() = if (paused || buffering) position else position + floor((time.now - since) * rate).toLong()

        /** Keeps the position reached so far; later changes count from now. */
        private fun rebase() {
            position = positionMs
            since = time.now
        }

        fun set(fields: JsonObject) {
            rebase()
            for ((k, v) in fields) {
                when (k) {
                    "player" -> {}
                    "positionMs" -> position = v.asInt() ?: fail("positionMs $v")
                    "paused" -> paused = v.asBoolean() ?: fail("paused $v")
                    "buffering" -> buffering = v.asBoolean() ?: fail("buffering $v")
                    "durationMs" -> durationMs = if (v is JsonNull) null else v.asInt() ?: fail("durationMs $v")
                    "firstFrameShown" -> firstFrameShown = v.asBoolean() ?: fail("firstFrameShown $v")
                    "canSetRate" -> canSetRate = v.asBoolean() ?: fail("canSetRate $v")
                    "seekAccuracyMs" -> seekAccuracyMs = v.asInt() ?: fail("seekAccuracyMs $v")
                    "playThrows" -> playThrows = v.asBoolean() ?: fail("playThrows $v")
                    else -> fail("unknown player member $k")
                }
            }
        }

        override suspend fun play() = command(obj("do" to "play"), playThrows) {
            rebase()
            paused = false
        }

        override suspend fun pause() = command(obj("do" to "pause")) {
            rebase()
            paused = true
        }

        override suspend fun seek(positionMs: Long) = command(obj("do" to "seek", "positionMs" to positionMs)) {
            rebase()
            position = positionMs
        }

        override suspend fun setRate(rate: Double) = command(obj("do" to "setRate", "rate" to rate)) {
            rebase()
            this.rate = rate
        }

        private suspend fun command(command: JsonObject, throws: Boolean = false, apply: () -> Unit) {
            out.command += JsonObject(mapOf("player" to JsonPrimitive(id)) + command)
            if (holdNext) {
                holdNext = false
                val held = Held(apply)
                this.held = held
                try {
                    held.done.await()
                } catch (e: CancellationException) {
                    if (this.held === held) this.held = null
                    throw e
                }
                return
            }
            if (throws) throw IllegalStateException("$id refused to ${command["do"].asString()}")
            apply()
        }
    }

    private fun player(id: String?): FakePlayer = players[id] ?: fail("unknown player $id")

    // -- Steps --------------------------------------------------------------------------------------------------------

    /** Runs one step and settles. */
    fun step(s: JsonObject) {
        val on = s["on"]?.let { it.asString() ?: fail("on $it") }
        when {
            "call" in s -> call(s)
            "recv" in s -> socket(on).deliver(s.getValue("recv").toString())
            "recvText" in s -> socket(on).deliver(s.string("recvText"))
            "drop" in s -> socket(on).drop(s["drop"].let { if (it is JsonNull) null else it.asInt()?.toInt() ?: fail("drop $it") })
            "nextOpen" in s -> nextOpen[on ?: ""] = s.string("nextOpen").takeIf { it == "fail" || it == "hang" } ?: fail("nextOpen ${s["nextOpen"]}")
            "rtt" in s -> {
                val ms = s.longOrNull("rtt")
                if (on == null) {
                    rttAll = ms
                    rtt.clear()
                } else {
                    rtt[on] = ms
                }
            }
            "nextGet" in s -> nextGet = s["nextGet"]
            "advance" in s -> {
                time.advance(s.long("advance"))
                return
            }
            "player" in s -> players.getOrPut(s.string("player")) { FakePlayer(s.string("player")) }.set(s)
            "hold" in s -> player(s.string("hold")).holdNext = true
            "finish" in s -> {
                val p = player(s.string("finish"))
                val held = p.held ?: fail("${s["finish"]} has no running command to finish")
                p.held = null
                if (s["throws"].asBoolean() == true) {
                    held.done.completeExceptionally(IllegalStateException("${s["finish"]} failed"))
                } else {
                    held.apply()
                    held.done.complete(Unit)
                }
            }
            "answer" in s || "answerThrows" in s -> {
                val pending = resolving ?: fail("no resolve call to answer")
                resolving = null
                val answer = s["answer"]
                if (answer != null) {
                    pending.answer.complete(answer.asArray()!!.map { candidate(it) })
                } else {
                    pending.answer.completeExceptionally(IllegalStateException("the resolver failed"))
                }
            }
            "probeAnswer" in s || "probeThrows" in s -> {
                val id = (s["probeAnswer"] ?: s["probeThrows"]).asString()
                val pending = probes.remove(id) ?: fail("no running probe of $id")
                if ("probeAnswer" in s) {
                    pending.answer.complete(s.longOrNull("durationMs"))
                } else {
                    pending.answer.completeExceptionally(IllegalStateException("the probe of $id failed"))
                }
            }
            else -> fail("unknown step $s")
        }
        time.settle()
    }

    private fun call(s: JsonObject) {
        val name = s.string("call")
        val id = s["id"].asString()
        if (id != null) calls += id
        fun ok(value: JsonElement?) {
            if (id != null) out.done += if (value == null) obj("id" to id) else obj("id" to id, "value" to value)
        }
        fun failed(e: Throwable) {
            when {
                e !is PartyException -> problems += "$name threw $e"
                id == null -> problems += "$name without an id threw ${e.code}"
                else -> out.done += obj("id" to id, "error" to e.code, "re" to e.re, "message" to e.message)
            }
        }
        fun later(block: suspend () -> JsonElement?) {
            scope.launch(start = CoroutineStart.UNDISPATCHED) {
                val value = try {
                    block()
                } catch (e: Throwable) {
                    failed(e)
                    return@launch
                }
                ok(value)
            }
        }
        val party = party
        try {
            when (name) {
                "create" -> return later { JsonPrimitive(party.create()) }
                "join" -> {
                    val create = s["create"]?.let { it.asBoolean() ?: fail("create $it") }
                    return later {
                        if (create == null) party.join(s.string("code")) else party.join(s.string("code"), create)
                        null
                    }
                }
                "choose" -> {
                    val c = candidate(s["candidate"])
                    return later { JsonPrimitive(party.choose(c)) }
                }
                "detachPlayer" -> {
                    val released = party.detachPlayer(s["player"]?.let { player(it.asString()) })
                    return later {
                        released.join()
                        null
                    }
                }
                "close" -> {
                    val released = party.close()
                    return later {
                        released.join()
                        null
                    }
                }
                "refresh" -> return later { JsonPrimitive(relays.refresh()) }
                "attachPlayer" -> party.attachPlayer(player(s.string("player")), s.string("ref"))
                "setMedia" -> {
                    val media = readMedia(s["media"].asObject() ?: fail("media ${s["media"]}")) ?: fail("no Kotlin Media for ${s["media"]}")
                    party.setMedia(media, s.string("ref"), s.longOrNull("positionMs") ?: 0)
                }
                "seek" -> party.seek(s.long("ms"))
                "setRate" -> party.setRate(s["rate"].asNumber() ?: fail("rate ${s["rate"]}"))
                "setOffset" -> party.setOffset(s.long("ms"))
                "setControl" -> party.setControl(Control.entries.firstOrNull { it.wire == s.string("mode") } ?: fail("mode ${s["mode"]}"))
                "lock" -> party.lock(s["locked"].asBoolean() ?: fail("locked ${s["locked"]}"))
                "kick" -> party.kick(s.string("peerId"))
                "transferHost" -> party.transferHost(s.string("peerId"))
                "leave" -> party.leave()
                "end" -> party.end()
                "clearMedia" -> party.clearMedia()
                "play" -> party.play()
                "pause" -> party.pause()
                "acceptMismatch" -> party.acceptMismatch()
                "declineMismatch" -> party.declineMismatch()
                "resolveAgain" -> party.resolveAgain()
                "playerChanged" -> party.playerChanged()
                else -> fail("unknown call $name")
            }
            ok(null)
        } catch (e: PartyException) {
            failed(e)
        }
    }

    /**
     * This step's outputs as [want] writes them: a sent message with its connection only where [want] pins it, a
     * call's error with `re` and `message` only where [want]'s entry of that call has them, and the state fields [want]
     * names.
     */
    fun take(want: JsonObject): JsonObject {
        val got = buildMap {
            for ((name, list) in out.channels()) {
                if (list.isEmpty()) continue
                val written = want[name].asArray().orEmpty().map { it.asObject() }
                put(
                    name,
                    JsonArray(
                        list.mapIndexed { i, e ->
                            val id = e.asObject()?.get("id")
                            shape(name, e, if (name == "done") written.firstOrNull { it?.get("id") == id } else written.getOrNull(i))
                        },
                    ),
                )
            }
            val fields = want["state"].asObject()?.keys.orEmpty()
            if (fields.isNotEmpty()) put("state", JsonObject(fields.associateWith { projectState(it) }))
        }
        out = Outputs()
        return JsonObject(got)
    }

    private fun shape(channel: String, e: JsonElement, written: JsonObject?): JsonElement {
        val o = e.asObject() ?: return e
        return when {
            channel == "send" -> if (written != null && "on" in written && "type" !in written) o else o.getValue("msg")
            channel == "done" && "error" in o -> JsonObject(o.filterKeys { (it != "re" && it != "message") || (written != null && it in written) })
            else -> o
        }
    }

    /** A state field as the README's projection writes it. */
    private fun projectState(field: String): JsonElement {
        val state = party.state.value
        return when (field) {
            "connection" -> json(state.connection.name.lowercase())
            "peerId" -> json(state.peerId)
            "room" -> json(state.room?.rev)
            "media" -> json(state.media?.title)
            "match" -> json(state.match?.let { m -> if (m.deltaMs == null) obj("state" to m.state.wire) else obj("state" to m.state.wire, "deltaMs" to m.deltaMs) })
            "choice" -> json(state.choice?.let { obj("id" to it.id, "ref" to it.ref) })
            "mismatch" -> json(state.mismatch?.let { obj("id" to it.candidate.id, "deltaMs" to it.deltaMs) })
            "candidates" -> JsonArray(state.candidates.map { obj("id" to it.candidate.id, "deltaMs" to it.deltaMs) })
            "offsetMs" -> json(state.offsetMs)
            "player" -> json(state.player.wire)
            "endReason" -> json(state.endReason?.name?.lowercase())
            "isHost" -> json(state.isHost)
            "canControl" -> json(state.canControl)
            "roomPositionMs" -> json(party.roomPositionMs())
            else -> fail("unknown state field $field")
        }
    }

    private fun project(e: PartyEvent): JsonElement = when (e) {
        is PartyEvent.Action ->
            obj("type" to "action", "kind" to e.kind, "byPeerId" to e.byPeerId, "by" to e.by?.peerId, "byMe" to e.byMe, "room" to e.room.rev)
        is PartyEvent.ParticipantJoined -> obj("type" to "participantJoined", "peerId" to e.participant.peerId)
        is PartyEvent.ParticipantLeft -> obj("type" to "participantLeft", "peerId" to e.participant.peerId)
        is PartyEvent.Error -> obj("type" to "error", "code" to e.error.code, "re" to e.error.re)
        is PartyEvent.ResolveFailed -> obj("type" to "resolveFailed", "timeout" to (e.error is TimeoutCancellationException))
        is PartyEvent.PlayBlocked -> obj("type" to "playBlocked")
        is PartyEvent.Ended -> obj("type" to "ended", "reason" to e.reason.name.lowercase(), "error" to e.error?.code)
        else -> json("unknown event $e")
    }

    /** Closes the party without checking more, then stops every coroutine of the trace. */
    fun close() {
        party.close()
        time.settle()
        job.cancel()
        time.settle()
        http.close()
        engine.close()
    }
}

// -- JSON -----------------------------------------------------------------------------------------------------------

private fun JsonObject.string(key: String): String = get(key).asString() ?: fail("$key is ${get(key)}")

private fun JsonObject.long(key: String): Long = get(key).asInt() ?: fail("$key is ${get(key)}")

private fun JsonObject.longOrNull(key: String): Long? = when (val v = get(key)) {
    null, is JsonNull -> null
    else -> v.asInt() ?: fail("$key is $v")
}

private fun json(value: Any?): JsonElement = when (value) {
    null -> JsonNull
    is JsonElement -> value
    is String -> JsonPrimitive(value)
    is Number -> JsonPrimitive(value)
    is Boolean -> JsonPrimitive(value)
    else -> fail("no JSON for $value")
}

private fun obj(vararg members: Pair<String, Any?>): JsonObject = JsonObject(members.associate { (k, v) -> k to json(v) })

/**
 * [e] in one form, so that equal JSON compares equal: members sorted, an integral number as an int, and a `setRate`
 * command's `rate` rounded to 6 decimals.
 */
private fun canonical(e: JsonElement?): JsonElement = when (e) {
    null, is JsonNull -> JsonNull
    is JsonObject -> JsonObject(
        e.keys.sorted().associateWith { k ->
            val v = e.getValue(k)
            if (k == "rate" && e["do"].asString() == "setRate") json(((v.asNumber() ?: fail("rate $v")) * 1e6).roundToLong() / 1e6) else canonical(v)
        },
    )
    is JsonArray -> JsonArray(e.map { canonical(it) })
    is JsonPrimitive -> when {
        e.isString -> e
        e.asInt() != null -> JsonPrimitive(e.asInt())
        else -> e.asNumber()?.let { JsonPrimitive(it) } ?: e
    }
}

/**
 * [o] with the order the README doesn't pin taken out: `net` in a stable sort by URL (each URL's own order stays) and
 * `done` sorted by call id.
 */
private fun unordered(o: JsonObject): JsonObject {
    fun url(e: JsonElement): String = e.asObject()?.let { it["get"] ?: it["open"] ?: it["close"] }.asString().orEmpty()
    fun id(e: JsonElement): String = e.asObject()?.get("id").asString().orEmpty()
    val net = o["net"].asArray()?.let { "net" to JsonArray(it.sortedBy(::url)) }
    val done = o["done"].asArray()?.let { "done" to JsonArray(it.sortedBy(::id)) }
    return JsonObject(o + listOfNotNull(net, done))
}
