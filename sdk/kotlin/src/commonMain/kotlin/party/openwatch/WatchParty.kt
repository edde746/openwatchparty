package party.openwatch

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CompletableJob
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.cancel
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.delay
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.joinAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import kotlinx.coroutines.withTimeoutOrNull
import kotlin.coroutines.ContinuationInterceptor
import kotlin.math.abs
import kotlin.math.floor
import kotlin.math.min
import kotlin.random.Random

/**
 * One participant's session (SDK.md §16.2).
 *
 * The party holds the connection to the room's relay and the room state. It matches the room's media to the user's own
 * file through the [Resolver], sends `status` reports, and drives the attached [PlayerAdapter] so it follows the room.
 *
 * The party runs in a child [SupervisorJob] of [scope], on its dispatcher, and calls the adapter and the resolver
 * there. Use a single-threaded scope, such as a Main scope on Android, and call the party from that thread.
 *
 * @param client the app's `<app>/<version>`, such as `Example/1.0`, from 1 to 64 characters. Everyone sees it, and
 *   the relay's terms use it.
 * @param name the user's display name, from 1 to 32 characters.
 * @param resolver finds the user's files for other participants' media. Without a resolver, the party never searches.
 *   Other participants' media then has no candidates, and it is unavailable from the start of its epoch.
 * @throws PartyException `invalid_message` when [client] or [name] breaks SPEC.md §6.3 or holds an unpaired surrogate.
 *   SPEC.md §6.3 counts lengths in code points, forbids some characters, and requires the `<app>/<version>` shape.
 */
public class WatchParty<T> internal constructor(
    private val relays: Relays,
    private val client: String,
    private val name: String,
    private val resolver: Resolver<T>?,
    scope: CoroutineScope,
    private val clock: MonotonicClock,
    private val random: Random,
    transport: Transport?,
) {
    public constructor(
        relays: Relays,
        client: String,
        name: String,
        resolver: Resolver<T>? = null,
        scope: CoroutineScope,
        clock: MonotonicClock = MonotonicClock.System,
        random: Random = Random.Default,
    ) : this(relays, client, name, resolver, scope, clock, random, null)

    init {
        nameProblem(name)?.let { throw PartyException(PartyException.INVALID_MESSAGE, "name $it") }
        clientProblem(client)?.let { throw PartyException(PartyException.INVALID_MESSAGE, "client $it") }
    }

    private val scope = CoroutineScope(scope.coroutineContext + SupervisorJob(scope.coroutineContext[Job]))
    private val dispatcher = scope.coroutineContext[ContinuationInterceptor] ?: Dispatchers.Default
    private val transport = transport ?: KtorTransport(relays.http, this.scope)

    private val _state = MutableStateFlow(PartyState<T>())
    private val _events = MutableSharedFlow<PartyEvent>(extraBufferCapacity = Channel.UNLIMITED)

    /** Everything the UI shows, which changes on events, never per tick. */
    public val state: StateFlow<PartyState<T>> = _state.asStateFlow()

    /**
     * What happens in the room, as events (SDK.md §15).
     *
     * The events report actions, participants coming and going, refusals, resolver failures, a player that refused to
     * start, and the end of the party.
     *
     * The flow never drops an event. Every subscriber gets every event emitted while it is subscribed, in order,
     * however slowly it reads.
     */
    public val events: SharedFlow<PartyEvent> = _events.asSharedFlow()

    // -- Session ------------------------------------------------------------------------------------------------------

    private var closed = false
    private var connection = Connection.IDLE
    private var endReason: EndReason? = null

    /** Bumped when a session starts or ends: work of an older one is dropped. */
    private var generation = 0

    /** The connection in use; while reconnecting, the dropped one, whose clock the steps still use. */
    private var link: Link? = null

    /** Own peer ID, from a `joined` until the party ends or leaves. */
    private var peerId: String? = null

    /** The `resume` of the last `joined`, for the reconnect (SDK.md §14). Never part of [state]: it holds the token. */
    private var resumeMessage: String? = null
    private var room: Room? = null

    private var epoch = 0L
    private var media: Media? = null
    /** The descriptor of this party's last `setMedia`, and the choice for it: the app's file (SDK.md §9.1). */
    private var own: OwnMedia<T>? = null
    /** The current epoch holds the party's own media, as decided when it started (SDK.md §9.1). */
    private var ownEpoch = false
    /** The text of this party's `setMedia` while it is pending (SDK.md §10.2), to send again after a resume (§14). */
    private var pendingSetMedia: String? = null

    private var match: Match? = null
    private var choice: Candidate<T>? = null
    private var mismatch: Mismatch<T>? = null
    private var candidates: List<Ranked<T>> = emptyList()
    private var matching: Job? = null

    /** The probe of the latest [choose] while it runs; cancelling matching cancels it. */
    private var manualProbe: Job? = null

    /** Bumped by every [choose] and every cancel of matching: a [choose] that started before drops its result. */
    private var chooseTurn = 0
    private var offsetMs = 0L

    private var tick: Job? = null
    private var reconnecting: Job? = null
    /** The running resume attempt sent `resume`: it outlives the end of the party until its answer (SDK.md §14). */
    private var resumeSent = false

    /** Re-checks the running resume attempt's answer; the end calls it, so a `joined` that waits for a clock leaves. */
    private var resumeCheck: (() -> Unit)? = null
    private var resumeWindow: Job? = null

    // -- Player -------------------------------------------------------------------------------------------------------

    private var adapter: PlayerAdapter? = null
    /** The `ref` of the file [adapter] has open; the adapter is bound only while it is the choice's (SDK.md §10.2). */
    private var attachedRef: T? = null
    private var away = false
    private var bound = false
    /** The bound adapter refused to start: a `play` the SDK issued threw and it read paused (SDK.md §10.2). */
    private var blocked = false
    private var appliedRate = 1.0
    private var nudging = false
    private var lastSeekAtMs: Long? = null
    private var playerState = PlayerState.IDLE
    /** The adapters with a command queued or running, each with its own lane (SDK.md §11.1). */
    private val lanes = mutableListOf<Lane>()
    /** Completes once no released adapter's lane is busy; null while nobody waits for that. */
    private var drained: CompletableJob? = null
    /** A step was asked for while the attached adapter's commands ran; it runs once they are done (SDK.md §11.1). */
    private var stepWanted = false
    private var playTimer: Job? = null

    private var holdTimer: Job? = null
    private var holdRe: String? = null

    // -- Status -------------------------------------------------------------------------------------------------------

    private var lastStatus: StatusFields? = null
    private var lastStatusAtMs: Long? = null
    private var statusTimer: Job? = null
    private var statusForced = false
    private var heartbeat: Job? = null

    /** The running `create` or `join`, its result, and whether it sent `create` or `join` already. */
    private var startJob: Job? = null
    private var startResult: CompletableDeferred<*>? = null
    private var startSent = false

    // -- Create and join ----------------------------------------------------------------------------------------------

    /**
     * Creates a room and returns its code (SDK.md §6).
     *
     * With several relays, the party creates the room on the first relay that answers a ping. When a relay answers
     * `relay_full`, the party passes `create` on to the next relay in list order that it hasn't gone to yet. When every
     * relay is full, the call fails with the last `relay_full`.
     */
    public suspend fun create(): String = attempt { myGeneration ->
        val list = relays.list
        val (first, firstLink) = if (list.size == 1) 0 to connect(list[0].url) else race(list)
        val untried = list.filterIndexed { i, _ -> i != first }.iterator()
        var link = firstLink
        var code: String? = null
        while (code == null) {
            code = try {
                admitNew(link, Out.create(name, client), "create", myGeneration)
            } catch (e: PartyException) {
                if (e.code != PartyException.RELAY_FULL || !untried.hasNext()) throw e
                link = connect(untried.next().url)
                null
            }
        }
        code
    }

    /**
     * Connects to every relay of [list] at once (SDK.md §6) and returns the index of the first whose connection yields
     * a clock sample, with that connection, whose clock goes on as the room's. Every other connection and attempt is
     * closed with 1000. Fails with `unreachable` when every connection failed or none had a sample within the connect
     * timeout.
     */
    private suspend fun race(list: List<Relay>): Pair<Int, Link> {
        val won = CompletableDeferred<Pair<Int, Link>>()
        val links = arrayOfNulls<Link>(list.size)
        var failures = 0
        fun failed() {
            if (++failures == list.size) won.completeExceptionally(PartyException(PartyException.UNREACHABLE, "no relay could be reached"))
        }
        val opening = list.mapIndexed { i, relay ->
            scope.launch {
                val socket = try {
                    transport.open(relay.url)
                } catch (e: CancellationException) {
                    throw e
                } catch (e: Exception) {
                    null
                }
                if (socket == null) {
                    failed()
                    return@launch
                }
                links[i] = Link(relay.url, socket, scope, clock).also { link ->
                    link.onPong = { if (link.sync.best != null) won.complete(i to link) }
                    link.onClosed = { failed() }
                    link.start()
                }
            }
        }
        var winner: Link? = null
        try {
            val result = withTimeoutOrNull(Owp.CONNECT_TIMEOUT_MS) { won.await() }
                ?: throw PartyException(PartyException.UNREACHABLE, "no relay answered in time")
            winner = result.second.apply {
                onPong = {}
                onClosed = {}
            }
            return result
        } finally {
            opening.forEach { it.cancel() }
            links.forEach { if (it !== winner) it?.close(1000) }
        }
    }

    /**
     * Joins the room [code] on the relay that owns it (SDK.md §4).
     *
     * [code] may be typed or pasted, so `k7m-q2x` works too. The party checks the code first, so `invalid_code` comes
     * before `already_in_room`.
     *
     * With [create], the relay makes a room with [code] when no room has it, and the party is that room's host (SDK.md
     * §7.2). An app uses it to bring a group back to a code it used before, such as one from a list of recent rooms.
     * Such a join may also fail with `relay_full`, which is final: unlike `create()`, `join` tries no other relay. A
     * relay that predates `create` answers `room_not_found`.
     */
    public suspend fun join(code: String, create: Boolean = false) {
        requireOpen()
        val normalized = normalizeCode(code) ?: throw PartyException(PartyException.INVALID_CODE, "not a room code")
        attempt { myGeneration ->
            var refreshed = false
            var relay = route(normalized, relays.list)
            if (relay == null) {
                relays.refresh()
                refreshed = true
                relay = route(normalized, relays.list) ?: throw PartyException(PartyException.ROOM_NOT_FOUND)
            }
            try {
                joinAt(relay, normalized, create, myGeneration)
            } catch (e: PartyException) {
                if (e.code != PartyException.ROOM_NOT_FOUND || refreshed) throw e
                relays.refresh()
                val moved = route(normalized, relays.list)
                if (moved == null || moved.url == relay.url) throw e
                joinAt(moved, normalized, create, myGeneration)
            }
        }
    }

    private suspend fun joinAt(relay: Relay, code: String, create: Boolean, myGeneration: Int) {
        val link = connect(relay.url)
        admitNew(link, Out.join(code, name, client, create), "join", myGeneration)
    }

    /**
     * Runs a `create` or `join` as [startJob] on the party's scope and waits for it. [leave] and [close] fail the wait at
     * once with `cancelled` (SDK.md §7.2); so does cancelling the caller.
     */
    private suspend fun <R> attempt(block: suspend (myGeneration: Int) -> R): R {
        val result = CompletableDeferred<R>()
        val myGeneration = withContext(dispatcher) {
            val myGeneration = begin()
            startResult = result
            startJob = scope.launch {
                try {
                    result.complete(block(myGeneration))
                } catch (e: Throwable) {
                    result.completeExceptionally(failed(myGeneration, e))
                    if (e is CancellationException) throw e
                }
            }
            myGeneration
        }
        try {
            return result.await()
        } catch (e: CancellationException) {
            withContext(NonCancellable + dispatcher) {
                if (myGeneration == generation && connection == Connection.CONNECTING) cancelAttempt()
            }
            throw e
        }
    }

    /**
     * `leave` or `close` during `create` or `join`: back to idle without an event; the attempt stops, except that a
     * connection that sent `create` or `join` waits for its answer and leaves a room it joined.
     */
    private fun cancelAttempt() {
        val job = startJob
        val result = startResult
        if (!startSent) job?.cancel()
        resetSession()
        connection = Connection.IDLE
        publish()
        result?.completeExceptionally(PartyException(PartyException.CANCELLED, "left while connecting"))
    }

    private fun requireOpen() {
        check(!closed) { "the party is closed" }
    }

    private fun requireStartable() {
        requireOpen()
        if (connection == Connection.CONNECTING || connection == Connection.CONNECTED || connection == Connection.RECONNECTING) {
            throw PartyException(PartyException.ALREADY_IN_ROOM, "the party is in a room or connecting")
        }
    }

    /**
     * Starts over for a new `create` or `join`: away is cleared, and an attached adapter stays attached into the new
     * session (SDK.md §7.2).
     */
    private fun begin(): Int {
        requireStartable()
        resetSession()
        connection = Connection.CONNECTING
        endReason = null
        publish()
        return generation
    }

    /**
     * A failed `create` or `join`: back to idle when the attempt is still current, keeping the adapter, and the error to
     * report.
     */
    private fun failed(myGeneration: Int, e: Throwable): Throwable {
        if (myGeneration == generation && connection == Connection.CONNECTING) {
            link?.close()
            resetSession()
            connection = Connection.IDLE
            publish()
        }
        return when (e) {
            is PartyException, is IllegalStateException -> e
            is CancellationException -> PartyException(PartyException.CANCELLED, "cancelled")
            else -> PartyException(PartyException.UNREACHABLE, e.message ?: "connection failed", cause = e)
        }
    }

    private suspend fun connect(url: String): Link {
        val socket = try {
            withTimeoutOrNull(Owp.CONNECT_TIMEOUT_MS) { transport.open(url) }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            throw PartyException(PartyException.UNREACHABLE, "could not connect to $url", cause = e)
        } ?: throw PartyException(PartyException.UNREACHABLE, "connecting to $url timed out")
        return Link(url, socket, scope, clock).also { it.start() }
    }

    /**
     * Sends [text] (`create`, `join` or a reconnect's `resume`) and waits up to the connect timeout for `joined` or a
     * refusal of [type]. While [needClock] returns true, also for the connection's first clock sample; [resumeCheck]
     * re-checks that wait. [admitted] runs once both are in, before any later message of [link] is handled.
     */
    private suspend fun admit(
        link: Link,
        text: String,
        type: String,
        needClock: (() -> Boolean)?,
        admitted: (In.Joined) -> Unit,
    ) {
        if (!link.isOpen) throw PartyException(PartyException.UNREACHABLE, "connection closed")
        val done = CompletableDeferred<Unit>()
        var joined: In.Joined? = null
        val later = mutableListOf<In>()
        fun finish() {
            val j = joined ?: return
            if (done.isCompleted || (needClock?.invoke() == true && link.sync.best == null)) return
            admitted(j)
            done.complete(Unit)
            later.forEach { handle(link, it) }
        }
        link.onMessage = { m ->
            when {
                done.isCompleted -> handle(link, m)
                joined != null -> later += m
                m is In.Joined -> {
                    joined = m
                    finish()
                }
                m is In.MalformedJoined -> {
                    // A relay fault (SDK.md §8): leave a room it may have made us a member of; a resume just fails.
                    if (type != "resume") link.send(Out.leave())
                    done.completeExceptionally(PartyException(PartyException.INVALID_MESSAGE, "the relay sent a malformed joined"))
                }
                m is In.Error && m.re == type -> done.completeExceptionally(PartyException(m.code, m.message, m.re))
            }
        }
        link.onPong = { finish() }
        link.onClosed = { code ->
            if (!done.isCompleted) done.completeExceptionally(PartyException(PartyException.UNREACHABLE, "connection closed ($code)"))
        }
        val check: () -> Unit = { finish() }
        if (needClock != null) resumeCheck = check
        link.send(text)
        val answered = try {
            withTimeoutOrNull(Owp.CONNECT_TIMEOUT_MS) { done.await() } != null
        } finally {
            if (resumeCheck === check) resumeCheck = null
        }
        if (!answered) {
            link.close()
            throw PartyException(PartyException.UNREACHABLE, "no answer to $type")
        }
    }

    /**
     * `create` or `join` on [link]; on `joined` the party becomes a member, and this returns the room code. Once the
     * attempt is cancelled, a `joined` leaves at once.
     */
    private suspend fun admitNew(link: Link, text: String, type: String, myGeneration: Int): String {
        if (myGeneration != generation) {
            link.close(1000)
            throw PartyException(PartyException.CANCELLED, "left while connecting")
        }
        this.link = link
        startSent = true
        var code = ""
        try {
            admit(link, text, type, needClock = null) { joined ->
                code = joined.code
                if (myGeneration != generation) {
                    // Cancelled while the relay answered: don't stay behind as a disconnected participant.
                    link.send(Out.leave())
                    link.close(1000)
                } else {
                    joinedOn(link, joined, resumed = false)
                }
            }
        } catch (e: Throwable) {
            link.close(1000)
            if (myGeneration != generation) throw PartyException(PartyException.CANCELLED, "left while connecting")
            throw e
        } finally {
            if (myGeneration == generation) startSent = false
        }
        if (myGeneration != generation) throw PartyException(PartyException.CANCELLED, "left while connecting")
        return code
    }

    /** SDK.md §7.3: `joined` on a new connection, after `create`, `join` or a reconnect's `resume`. */
    private fun joinedOn(link: Link, joined: In.Joined, resumed: Boolean) {
        val previous = room
        this.link = link
        peerId = joined.peerId
        resumeMessage = Out.resume(joined.code, joined.peerId, joined.token)
        link.onMessage = { handle(link, it) }
        link.onPong = {}
        link.onClosed = { code -> linkClosed(link, code) }
        room = joined.room
        connection = Connection.CONNECTED
        resumeWindow?.cancel()
        resumeWindow = null
        if (resumed && previous != null) participantEvents(previous, joined.room)
        if (!resumed || joined.room.epoch != epoch) {
            pendingSetMedia = null
            newEpoch(joined.room.epoch, joined.media)
        }
        // A `setMedia` still pending never reached the room: as host the party sends it again after the status, else
        // it gives it up (SDK.md §14).
        val resend = pendingSetMedia?.takeIf { joined.room.participants.any { it.peerId == joined.peerId && it.host } }
        if (resend == null && pendingSetMedia != null) {
            pendingSetMedia = null
            rebind()
        }
        lastStatus = null
        lastStatusAtMs = null
        statusTimer?.cancel()
        statusTimer = null
        if (tick == null) tick = scope.every(Owp.TICK_MS) { onTick() }
        updatePlayer()
        publish()
        requestStatus(force = true)
        resend?.let { link.send(it) }
        step()
        refreshListInBackground()
    }

    /** Every `joined` may refresh the public list in the background (SDK.md §3.3). */
    private fun refreshListInBackground() {
        scope.launch { relays.refreshInBackground() }
    }

    // -- Relay messages -----------------------------------------------------------------------------------------------

    private fun handle(link: Link, message: In) {
        if (link !== this.link || connection != Connection.CONNECTED) return
        when (message) {
            is In.RoomMsg -> snapshot(link, message)
            is In.Error -> refused(message)
            is In.Closed -> {
                link.close(1000)
                end(
                    when (message.reason) {
                        "kicked" -> EndReason.KICKED
                        "expired" -> EndReason.EXPIRED
                        else -> EndReason.ENDED
                    },
                )
            }
            is In.Joined, is In.Pong, In.MalformedJoined -> {}
        }
    }

    /**
     * An accepted `room` (SDK.md §8) replaces the snapshot; one of a new epoch then starts it with the `media` it carries
     * (§9.1), so a bind there already follows the new snapshot. Then the participant events and the action it carries
     * (§15).
     */
    private fun snapshot(link: Link, message: In.RoomMsg) {
        val next = message.room
        val previous = room
        if (previous != null && next.rev <= previous.rev) return
        val starts = next.epoch != epoch
        if (starts && !message.carriesMedia) {
            // The party can't follow the room without the new epoch's media (SDK.md §8).
            link.send(Out.leave())
            end(EndReason.LOST, PartyException(PartyException.INVALID_MESSAGE, "the relay sent a new epoch without valid media"))
            return
        }
        room = next
        if (starts) {
            pendingSetMedia = null
            newEpoch(next.epoch, message.media)
        }
        if (previous != null) participantEvents(previous, next)
        val action = message.action
        if (action != null) {
            _events.tryEmit(
                PartyEvent.Action(
                    kind = action.kind,
                    byPeerId = action.by,
                    by = action.by?.let { by -> next.participants.firstOrNull { it.peerId == by } },
                    byMe = action.by != null && action.by == peerId,
                    room = next,
                ),
            )
            endHold()
        }
        updatePlayer()
        publish()
        requestStatus()
        step()
    }

    private fun participantEvents(previous: Room, next: Room) {
        val before = previous.participants.associateBy { it.peerId }
        val after = next.participants.associateBy { it.peerId }
        for ((id, p) in after) if (id !in before) _events.tryEmit(PartyEvent.ParticipantJoined(p))
        for ((id, p) in before) if (id !in after) _events.tryEmit(PartyEvent.ParticipantLeft(p))
    }

    private fun refused(error: In.Error) {
        val exception = PartyException(error.code, error.message, error.re)
        if (error.re != null && error.re == holdRe) endHold()
        if (error.re == "setMedia" && pendingSetMedia != null) {
            // The binding rule, then a step (SDK.md §10.2).
            pendingSetMedia = null
            rebind()
            updatePlayer()
            publish()
            requestStatus()
            step()
        }
        if (error.re != "create" && error.re != "join" && error.re != "resume") _events.tryEmit(PartyEvent.Error(exception))
    }

    // -- Media and matching (SDK.md §9) -------------------------------------------------------------------------------

    private fun newEpoch(newEpoch: Long, newMedia: Media?) {
        epoch = newEpoch
        media = newMedia
        cancelMatching()
        offsetMs = 0
        choice = null
        mismatch = null
        candidates = emptyList()
        unbind()
        ownEpoch = newMedia != null && newMedia == own?.media
        when {
            newMedia == null -> {
                match = null
                adapter?.let { command(it) { pause() } }
            }
            ownEpoch -> {
                match = Match(MatchState.MATCHED, 0)
                choice = own?.choice
            }
            else -> {
                adapter?.let { command(it) { pause() } }
                match = Match(MatchState.SEARCHING)
                startMatching(newMedia)
            }
        }
        rebind()
    }

    private fun cancelMatching() {
        matching?.cancel()
        matching = null
        chooseTurn++
        manualProbe?.cancel()
        manualProbe = null
    }

    /**
     * The resolver and the walk for [target] (SDK.md §9.2, §9.5), then the outcome (§9.6). Without a resolver there's no
     * search: the match is unavailable at once.
     */
    private fun startMatching(target: Media) {
        val resolver = resolver ?: run {
            match = Match(MatchState.UNAVAILABLE)
            return
        }
        matching = scope.launch {
            val found = resolve(resolver, target)
            val result = walk(target, found, clock) { ranked ->
                candidates = ranked
                publish()
            }
            when (val outcome = result.outcome) {
                is WalkOutcome.Matched -> {
                    choice = outcome.candidate
                    match = Match(MatchState.MATCHED, outcome.deltaMs)
                }
                is WalkOutcome.Mismatch -> {
                    mismatch = Mismatch(outcome.candidate, outcome.deltaMs)
                    match = Match(MatchState.SEARCHING, outcome.deltaMs)
                }
                WalkOutcome.Unavailable -> match = Match(MatchState.UNAVAILABLE)
            }
            matching = null
            rebind()
            updatePlayer()
            publish()
            requestStatus()
        }
    }

    /**
     * [resolveAgain] while a file is chosen (SDK.md §9.1): only the resolver and the ranking, whose result replaces the
     * candidates, each with a delta from its known duration; no probes, and nothing else changes. Without a resolver
     * there are no candidates.
     */
    private fun startRanking(target: Media) {
        val resolver = resolver ?: run {
            candidates = emptyList()
            return
        }
        matching = scope.launch {
            val found = resolve(resolver, target)
            candidates = rank(target, found).map { c -> Ranked(c, countedDuration(c.durationMs)?.let { it - target.durationMs }) }
            matching = null
            publish()
        }
    }

    /**
     * [resolver]'s candidates for [target]; none when it throws or takes longer than 30 s, which
     * [PartyEvent.ResolveFailed] reports.
     */
    private suspend fun resolve(resolver: Resolver<T>, target: Media): List<Candidate<T>> = try {
        withTimeout(Owp.RESOLVE_TIMEOUT_MS) { resolver.resolve(target) }
    } catch (e: Exception) {
        // The timeout, and a cancellation of the resolver's own such as its own timeout, are failures; only matching
        // being cancelled stops the search without an event (SDK.md §9.2).
        if (e is CancellationException) currentCoroutineContext().ensureActive()
        _events.tryEmit(PartyEvent.ResolveFailed(e))
        emptyList()
    }

    /** Plays the offered file, and `match` becomes mismatched (SDK.md §9.6). */
    public fun acceptMismatch() {
        val offered = mismatch ?: return
        choice = offered.candidate
        match = Match(MatchState.MISMATCHED, offered.deltaMs)
        mismatch = null
        rebind()
        updatePlayer()
        publish()
        requestStatus()
    }

    /**
     * Declines the offered file, which then doesn't play.
     *
     * `match` becomes unavailable, and the room stops waiting for this user.
     */
    public fun declineMismatch() {
        val offered = mismatch ?: return
        match = Match(MatchState.UNAVAILABLE, offered.deltaMs)
        mismatch = null
        choice = null
        rebind()
        updatePlayer()
        publish()
        requestStatus()
    }

    /**
     * Takes a file the user picked by hand (SDK.md §9.6), probing it when it has no duration.
     *
     * [candidate] becomes the choice, also when it has the current choice's id. A refreshed `ref` then replaces the old
     * one.
     *
     * Returns false, and changes nothing, when any of these holds:
     *
     * - The room has no media.
     * - The file has no duration (1 ms to 7 days) and no probe.
     * - The probe fails.
     *
     * The latest call wins. An earlier call that is still probing is cancelled and returns false. So is a call that is
     * probing when matching is cancelled, by a new epoch, by [resolveAgain] or by the end of the party.
     */
    public suspend fun choose(candidate: Candidate<T>): Boolean = withContext(dispatcher) {
        val target = media ?: return@withContext false
        manualProbe?.cancel()
        val myTurn = ++chooseTurn
        val duration = countedDuration(candidate.durationMs) ?: candidate.probe?.let { countedDuration(probeByHand(it)) }
        if (duration == null || myTurn != chooseTurn || media == null) return@withContext false
        cancelMatching()
        val delta = duration - target.durationMs
        choice = candidate
        mismatch = null
        match = Match(if (abs(delta) <= Owp.MATCH_TOLERANCE_MS) MatchState.MATCHED else MatchState.MISMATCHED, delta)
        rebind()
        updatePlayer()
        publish()
        requestStatus()
        true
    }

    /** Runs [probe] for [choose] as [manualProbe]; null when it fails or the SDK cancels it. */
    private suspend fun probeByHand(probe: suspend () -> Long?): Long? {
        val probing = scope.async { runProbe(probe).durationMs }
        manualProbe = probing
        return try {
            probing.await()
        } catch (e: CancellationException) {
            currentCoroutineContext().ensureActive() // the caller was cancelled: so is the probe, below
            null
        } finally {
            probing.cancel()
            if (manualProbe === probing) manualProbe = null
        }
    }

    /**
     * Asks the resolver again for the current media (SDK.md §9.1).
     *
     * It keeps what plays: the offset and the adapter stay as they are.
     *
     * - Without a choice, matching starts over, with the resolver and the walk.
     * - With a choice, only the resolver and the ranking run. Their result replaces `state.candidates`, for [choose].
     *   The choice and the match stay.
     *
     * It does nothing without media, or in an epoch of the party's own media.
     */
    public fun resolveAgain() {
        val target = media ?: return
        if (ownEpoch) return
        cancelMatching()
        if (choice == null) {
            match = Match(MatchState.SEARCHING)
            mismatch = null
            candidates = emptyList()
            startMatching(target)
        } else {
            startRanking(target)
        }
        updatePlayer()
        publish()
        requestStatus()
    }

    // -- The player (SDK.md §10.2, §11) -------------------------------------------------------------------------------

    /**
     * Tells the party that the user's [player] has the file [ref] open.
     *
     * [ref] is normally `state.choice.ref` as published. As host, it can also be the `ref` passed to [setMedia].
     *
     * The party follows the room on the player, which is called binding it (SDK.md §10.2). It binds the player whenever
     * all of these hold, and it re-checks them on every change:
     *
     * - [ref] equals the choice's `ref`.
     * - The match is matched or mismatched.
     * - No `setMedia` of this party is pending.
     *
     * So attach once per file the player opens. A file opened for a choice that has since changed stays unbound.
     *
     * Attaching another adapter releases the attached one, as [detachPlayer] does. Attaching the same adapter again
     * replaces its ref and keeps its queued commands.
     *
     * Attaching works while the party is idle or ended too. The adapter stays attached into the session that the next
     * [create] or [join] starts, also when that attempt fails. The party's end releases the adapter, so an app whose
     * player still has its file open after [PartyEvent.Ended] attaches it again.
     */
    public fun attachPlayer(player: PlayerAdapter, ref: T) {
        val old = adapter
        if (player !== old) {
            release()
            lastSeekAtMs = null
        }
        adapter = player
        attachedRef = ref
        away = false
        checkDrained()
        rebind()
        updatePlayer()
        publish()
        requestStatus()
        step()
    }

    /**
     * Tells the party that the user left the player, so the room stops waiting for this user.
     *
     * Without [player], this releases the attached adapter. With [player], it does so only when [player] is the one
     * attached. Otherwise nothing changes.
     *
     * A released adapter's commands that haven't started are dropped. If the SDK was nudging the adapter, the adapter
     * gets the room's rate back once (SDK.md §11.2).
     *
     * @return a job that completes once no released adapter has a command queued or running, or at once when none
     *   has. It never fails. Join it before disposing of the player.
     */
    public fun detachPlayer(player: PlayerAdapter? = null): Job {
        if (player == null || player === adapter) {
            release()
            away = true
            updatePlayer()
            publish()
            requestStatus()
        }
        return released()
    }

    /**
     * Tells the party that the player's state changed.
     *
     * Calling this is optional. Without it, the SDK sees the change on the next tick.
     */
    public fun playerChanged() {
        updatePlayer()
        publish()
        requestStatus()
        step()
    }

    /**
     * Whether the attached adapter plays the current choice (SDK.md §10.2): its ref is the choice's, the match is
     * matched or mismatched, and no `setMedia` of this party is pending.
     */
    private fun bindable(): Boolean {
        if (adapter == null || media == null || pendingSetMedia != null) return false
        val chosen = choice ?: return false
        val state = match?.state
        return chosen.ref == attachedRef && (state == MatchState.MATCHED || state == MatchState.MISMATCHED)
    }

    /** Binds or unbinds as [bindable] says now; called after every change of what it reads. */
    private fun rebind() {
        val want = bindable()
        if (want == bound) return
        if (want) bind() else unbind()
    }

    /** Binding (SDK.md §10.2): no nudging, the room's rate once, then a step. */
    private fun bind() {
        val player = adapter ?: return
        bound = true
        nudging = false
        val rate = room?.rate ?: 1.0
        appliedRate = rate
        if (player.canSetRate) command(player) { setRate(rate) }
        step()
    }

    /** Unbinding never touches the adapter's queued commands; it ends blocked (SDK.md §10.2). */
    private fun unbind() {
        bound = false
        blocked = false
        nudging = false
        playTimer?.cancel()
        playTimer = null
    }

    /**
     * Releases the attached adapter (SDK.md §11.2): its commands that haven't started are dropped, and while nudging it
     * gets the room's rate back once, as the rate is now. Its lane runs out on its own; nothing else reaches it.
     */
    private fun release() {
        val player = adapter ?: return
        lane(player)?.queue?.removeAll { !it.restore }
        if (nudging) {
            val rate = room?.rate ?: 1.0
            command(player, restore = true) { setRate(rate) }
        }
        unbind()
        adapter = null
        attachedRef = null
        stepWanted = false
    }

    /**
     * One adapter command. A [seek] is in flight from its queueing to its end; a [restore] stays queued through another
     * release of its adapter.
     */
    private class Command(val seek: Boolean, val restore: Boolean, val run: suspend PlayerAdapter.() -> Unit)

    /** One adapter's commands, run one at a time, in order, each awaited; lanes of different adapters never wait. */
    private class Lane(val player: PlayerAdapter) {
        val queue = ArrayDeque<Command>()
        var running: Command? = null
    }

    private fun lane(player: PlayerAdapter): Lane? = lanes.firstOrNull { it.player === player }

    /**
     * Queues a command on [player]'s lane. A failed command is ignored: the next step undoes it. Once the attached
     * adapter's lane is empty, the party reads the player again and runs a step asked for meanwhile.
     */
    private fun command(
        player: PlayerAdapter,
        seek: Boolean = false,
        restore: Boolean = false,
        block: suspend PlayerAdapter.() -> Unit,
    ) {
        val command = Command(seek, restore, block)
        lane(player)?.let {
            it.queue.addLast(command)
            return
        }
        // In [lanes] before the runner starts, so it counts as busy under an immediate dispatcher too.
        val lane = Lane(player)
        lane.queue.addLast(command)
        lanes += lane
        scope.launch {
            try {
                while (true) {
                    val next = lane.queue.removeFirstOrNull() ?: break
                    lane.running = next
                    try {
                        next.run(player)
                    } catch (e: CancellationException) {
                        throw e
                    } catch (e: Exception) {
                        // Undone by the next step.
                    } finally {
                        lane.running = null
                    }
                }
            } finally {
                lanes -= lane
                checkDrained()
            }
            if (player !== adapter) return@launch
            updatePlayer()
            publish()
            requestStatus()
            if (stepWanted) {
                stepWanted = false
                step()
            }
        }.invokeOnCompletion {
            // A runner cancelled before it started (the app cancelled the scope) leaves no busy lane behind.
            if (lanes.remove(lane)) checkDrained()
        }
    }

    /** Completes once no lane but the attached adapter's is busy (SDK.md §11.2); at once when none is. */
    private fun released(): Job {
        if (lanes.all { it.player === adapter }) return SETTLED
        return drained ?: Job().also { drained = it }
    }

    private fun checkDrained() {
        val waiting = drained ?: return
        if (lanes.all { it.player === adapter }) {
            drained = null
            waiting.complete()
        }
    }

    private fun seekCommand(player: PlayerAdapter, positionMs: Long) {
        lastSeekAtMs = clock.nowMs()
        command(player, seek = true) { seek(positionMs) }
    }

    /** A seek the SDK issued on [player] is queued or running. */
    private fun seeking(player: PlayerAdapter): Boolean =
        lane(player)?.let { lane -> lane.running?.seek == true || lane.queue.any { it.seek } } == true

    private fun onTick() {
        step()
        updatePlayer()
        publish()
        requestStatus()
    }

    /** The adapter's seek accuracy (SDK.md §11.2); a negative one counts as 0. */
    private fun PlayerAdapter.accuracyMs(): Long = maxOf(0L, seekAccuracyMs)

    /** One reconciliation step (SDK.md §11), when the adapter is bound and the room's media and clock are known. */
    private fun step() {
        val player = adapter ?: return
        val room = room ?: return
        val media = media ?: return
        if (!bound || holdRe != null) return
        val now = link?.relayNow() ?: return
        if (lane(player) != null) {
            stepWanted = true
            return
        }
        val out = reconcile(
            StepInput(
                phase = room.phase,
                positionMs = room.positionMs,
                atMs = room.atMs,
                rate = room.rate,
                durationMs = media.durationMs,
                nowMs = now,
                offsetMs = offsetMs,
                playerPositionMs = player.positionMs,
                paused = player.paused,
                buffering = player.buffering,
                playerDurationMs = player.durationMs,
                canSetRate = player.canSetRate,
                seekAccuracyMs = player.accuracyMs(),
                appliedRate = appliedRate,
                nudging = nudging,
                sinceSeekMs = lastSeekAtMs?.let { clock.nowMs() - it },
            ),
        )
        nudging = out.nudging
        playTimer?.cancel()
        playTimer = out.playInMs?.let { scope.after(it) { step() } }
        for (action in out.actions) {
            when (action) {
                StepAction.Pause -> command(player) { pause() }
                is StepAction.SetRate -> {
                    appliedRate = action.rate
                    command(player) { setRate(action.rate) }
                }
                is StepAction.Seek -> seekCommand(player, action.positionMs)
                StepAction.Play -> command(player) {
                    try {
                        play()
                    } catch (e: Exception) {
                        if (e is CancellationException) currentCoroutineContext().ensureActive()
                        refusedToPlay(this, e)
                    }
                }
            }
        }
        // A seek it queued makes the player `loading` (SDK.md §10.2), and `status` reports that (§10.3).
        updatePlayer()
        publish()
        requestStatus()
    }

    /** A `play` the SDK issued threw: the player refused to start when it still reads paused (SDK.md §10.2). */
    private fun refusedToPlay(player: PlayerAdapter, error: Exception) {
        if (blocked || player !== adapter || !bound || !player.paused || room?.phase != Phase.PLAYING) return
        blocked = true
        updatePlayer()
        publish()
        requestStatus()
        _events.tryEmit(PartyEvent.PlayBlocked(error))
    }

    /** The player state of SDK.md §10.2. */
    private fun computePlayerState(): PlayerState {
        if (media == null) return PlayerState.IDLE
        if (match?.state == MatchState.UNAVAILABLE) return PlayerState.IDLE
        if (away) return PlayerState.IDLE
        val player = adapter
        val room = room
        if (!bound || player == null || room == null) return PlayerState.LOADING
        if (!player.firstFrameShown || seeking(player)) return PlayerState.LOADING
        val now = link?.relayNow()
        // Without relay time the SDK can't place the player in a playing room (SDK.md §10.2).
        if (room.phase == Phase.PLAYING && now == null) return PlayerState.LOADING
        var held = room.phase != Phase.PLAYING
        val anchor = room.positionMs + offsetMs
        if (!held && now != null) held = now.toDouble() < localStartAt(room.atMs, anchor, room.rate)
        if (held) {
            val end = player.durationMs?.takeIf { it > 0 }?.let { maxOf(0L, it - Owp.END_MARGIN_MS) }
            val holdAt = maxOf(0L, anchor).let { if (end != null) min(it, end) else it }
            if (abs(player.positionMs - holdAt) > maxOf(Owp.HELD_SEEK_MS, player.accuracyMs())) return PlayerState.LOADING
        }
        if (blocked) return PlayerState.LOADING
        if (player.buffering) {
            val running = now != null && roomRunning(room.phase, room.atMs, now)
            return if (running && !held) PlayerState.BUFFERING else PlayerState.LOADING
        }
        return PlayerState.READY
    }

    private fun updatePlayer() {
        // Blocked ends once the player plays or the room stops playing (SDK.md §10.2).
        if (blocked && (adapter?.paused != true || room?.phase != Phase.PLAYING)) blocked = false
        playerState = if (connection == Connection.CONNECTED || connection == Connection.RECONNECTING) computePlayerState() else PlayerState.IDLE
    }

    // -- Status (SDK.md §10) ------------------------------------------------------------------------------------------

    private fun statusFields() = StatusFields(
        epoch = epoch,
        player = playerState,
        match = if (media != null) match else null,
        offsetMs = offsetMs,
        rttMs = link?.sync?.best?.rttMs,
    )

    /**
     * Sends `status` when it changed (or [force]), at most one per 250 ms except the first for a new epoch, which goes
     * out at once; never while reconnecting.
     */
    private fun requestStatus(force: Boolean = false) {
        if (connection != Connection.CONNECTED) return
        val fields = statusFields()
        val last = lastStatus
        val changed = last == null || last.copy(rttMs = null) != fields.copy(rttMs = null)
        if (!changed && !force && !statusForced) return
        val lastAt = lastStatusAtMs
        val since = lastAt?.let { clock.nowMs() - it }
        if (since == null || since >= Owp.STATUS_SPACING_MS || last?.epoch != fields.epoch) {
            sendStatus(fields)
        } else {
            if (force) statusForced = true
            if (statusTimer == null) {
                statusTimer = scope.after(Owp.STATUS_SPACING_MS - since) {
                    statusTimer = null
                    requestStatus()
                }
            }
        }
    }

    private fun sendStatus(fields: StatusFields) {
        val link = link ?: return
        link.send(Out.status(fields))
        lastStatus = fields
        lastStatusAtMs = clock.nowMs()
        statusForced = false
        statusTimer?.cancel()
        statusTimer = null
        heartbeat?.cancel()
        heartbeat = scope.after(Owp.STATUS_HEARTBEAT_MS) { requestStatus(force = true) }
    }

    // -- Local commands (SDK.md §12) ----------------------------------------------------------------------------------

    /** Asks the room to play, and the room starts everyone together. */
    public fun play() {
        requireController()
        send(Out.play())
    }

    /**
     * Pauses the room.
     *
     * The bound player pauses at once, and the pause carries its position, in the reference timeline.
     */
    public fun pause() {
        requireController()
        val player = adapter?.takeIf { bound }
        val position = player?.let { wirePosition(it.positionMs) }
        player?.let { command(it) { pause() } }
        hold("pause")
        send(Out.pause(position))
    }

    /** Seeks the room to [positionMs], a position in the user's own file. */
    public fun seek(positionMs: Long) {
        requireController()
        if (positionMs < 0) throw PartyException(PartyException.INVALID_MESSAGE, "positionMs is negative")
        val roomPositionMs = wirePosition(positionMs)
        val player = adapter?.takeIf { bound }
        player?.let { seekCommand(it, positionMs) }
        hold("seek")
        send(Out.seek(roomPositionMs))
        // The queued local seek is an SDK seek: the player is `loading` until it has run (SDK.md §10.2 rule 5, §12).
        if (player != null) {
            updatePlayer()
            publish()
            requestStatus()
        }
    }

    /**
     * max(0, [positionMs] − offset): a local position as `pause` and `seek` send it (SDK.md §12); throws when the
     * difference isn't an int.
     */
    private fun wirePosition(positionMs: Long): Long {
        if (positionMs !in -Owp.MAX_INT..Owp.MAX_INT || positionMs - offsetMs !in -Owp.MAX_INT..Owp.MAX_INT) {
            throw PartyException(PartyException.INVALID_MESSAGE, "the position − offset is beyond 2^53 − 1")
        }
        return maxOf(0L, positionMs - offsetMs)
    }

    /** Sets the room's rate, from 0.5 to 2.0, and the bound player plays at [rate]. */
    public fun setRate(rate: Double) {
        requireController()
        if (!(rate >= Owp.MIN_RATE && rate <= Owp.MAX_RATE)) throw PartyException(PartyException.INVALID_MESSAGE, "rate is outside 0.5–2.0")
        adapter?.takeIf { bound && it.canSetRate }?.let { player ->
            appliedRate = rate
            command(player) { setRate(rate) }
        }
        hold("rate")
        send(Out.rate(rate))
    }

    /**
     * Sets the offset for a file that differs from the reference (SDK.md §13).
     *
     * The local position is the room position plus [offsetMs]. The offset is at most 604800000 (7 days) either way.
     * Any other value throws `invalid_message` and changes nothing.
     */
    public fun setOffset(offsetMs: Long) {
        if (offsetMs !in -Owp.MAX_OFFSET_MS..Owp.MAX_OFFSET_MS) {
            throw PartyException(PartyException.INVALID_MESSAGE, "offsetMs is beyond ${Owp.MAX_OFFSET_MS} either way")
        }
        this.offsetMs = offsetMs
        updatePlayer()
        publish()
        requestStatus()
        step()
    }

    /**
     * Shares [media] with the room, as host.
     *
     * The room starts at [positionMs] of the host's own file. [ref] is the app's value that reopens that file.
     *
     * Whenever the room plays [media], also after this party hands over host, `state.choice` is a candidate with the
     * id `""`, [ref] and the descriptor's duration (SDK.md §9.1). The app opens it like any choice. A player already
     * attached with [ref] binds without reopening.
     *
     * The party keeps its own copy of [media], so changing the app's maps or JSON afterwards changes nothing.
     */
    public fun setMedia(media: Media, ref: T, positionMs: Long = 0) {
        requireMember()
        requireHost()
        val shared = ownCopy(media)
        checkMedia(shared)?.let { throw PartyException(PartyException.INVALID_MESSAGE, it) }
        if (positionMs !in 0..Owp.MAX_INT) throw PartyException(PartyException.INVALID_MESSAGE, "positionMs is negative or beyond 2^53 − 1")
        own = OwnMedia(shared, Candidate(id = "", ref = ref, durationMs = shared.durationMs))
        val text = Out.setMedia(shared.toJson(), positionMs)
        pendingSetMedia = text
        rebind()
        updatePlayer()
        publish()
        send(text)
        // Unbinding may change the reported player (SDK.md §10.3).
        requestStatus()
    }

    public fun clearMedia() {
        requireMember()
        requireHost()
        send(Out.clearMedia())
    }

    public fun setControl(mode: Control) {
        requireMember()
        requireHost()
        send(Out.setControl(mode))
    }

    public fun transferHost(peerId: String) {
        requireMember()
        requireHost()
        requireParticipant(peerId)
        send(Out.transferHost(peerId))
    }

    public fun kick(peerId: String) {
        requireMember()
        requireHost()
        requireParticipant(peerId)
        send(Out.kick(peerId))
    }

    public fun lock(locked: Boolean) {
        requireMember()
        requireHost()
        send(Out.lock(locked))
    }

    /**
     * Ends the room for everyone, as host.
     *
     * The party ends when the relay confirms.
     */
    public fun end() {
        requireMember()
        requireHost()
        send(Out.end())
    }

    private fun send(text: String) {
        link?.send(text)
    }

    private fun requireMember() {
        if (connection != Connection.CONNECTED) throw PartyException(PartyException.NOT_IN_ROOM, "not connected to a room")
    }

    private fun requireHost() {
        if (room?.participants?.firstOrNull { it.peerId == peerId }?.host != true) {
            throw PartyException(PartyException.FORBIDDEN, "only the host may do this")
        }
    }

    private fun requireController() {
        requireMember()
        if (media == null) throw PartyException(PartyException.INVALID_MESSAGE, "the room has no media")
        val me = room?.participants?.firstOrNull { it.peerId == peerId }
        if (me?.host != true && room?.control != Control.EVERYONE) {
            throw PartyException(PartyException.FORBIDDEN, "only the host controls playback")
        }
    }

    private fun requireParticipant(peerId: String) {
        if (room?.participants?.none { it.peerId == peerId } != false) {
            throw PartyException(PartyException.INVALID_MESSAGE, "no participant $peerId")
        }
    }

    /** No step until the relay's `room` carries an action, it refuses the command, or 2 s pass (SDK.md §12). */
    private fun hold(type: String) {
        holdRe = type
        holdTimer?.cancel()
        holdTimer = scope.after(Owp.COMMAND_HOLD_MS) { endHold() }
    }

    private fun endHold() {
        if (holdRe == null) return
        holdRe = null
        holdTimer?.cancel()
        holdTimer = null
        step()
    }

    // -- Reconnect and resume (SDK.md §14) ----------------------------------------------------------------------------

    private fun linkClosed(closedLink: Link, closeCode: Int?) {
        if (closedLink !== link || connection != Connection.CONNECTED) return
        if (closeCode == CLOSE_REPLACED) {
            end(EndReason.REPLACED)
            return
        }
        connection = Connection.RECONNECTING
        statusTimer?.cancel()
        statusTimer = null
        heartbeat?.cancel()
        heartbeat = null
        endHold()
        updatePlayer()
        publish()
        val myGeneration = generation
        val resumeUrl = closedLink.url
        val resume = resumeMessage!!
        resumeWindow = scope.after(Owp.RESUME_WINDOW_MS) {
            if (myGeneration == generation && connection == Connection.RECONNECTING) {
                end(EndReason.LOST, PartyException(PartyException.UNREACHABLE, "could not resume within the resume window"))
            }
        }
        reconnecting = scope.launch {
            var attempt = 0
            while (myGeneration == generation) {
                attempt++
                val backoff = min(Owp.BACKOFF_MAX_MS, Owp.BACKOFF_START_MS shl min(attempt - 1, 10))
                delay(floor(backoff * (0.5 + 0.5 * random.nextDouble())).toLong())
                val socket = try {
                    withTimeoutOrNull(Owp.CONNECT_TIMEOUT_MS) { transport.open(resumeUrl) }
                } catch (e: CancellationException) {
                    throw e
                } catch (e: Exception) {
                    null
                } ?: continue
                val next = Link(resumeUrl, socket, scope, clock).also { it.start() }
                // Once `resume` is sent, the attempt outlives the end of the party: a `joined` gets `leave` (SDK.md §14).
                resumeSent = true
                try {
                    // The clock sample matters only for going on: once the party ended, a `joined` leaves at once.
                    admit(next, resume, "resume", needClock = { myGeneration == generation }) { joined ->
                        if (myGeneration == generation) {
                            joinedOn(next, joined, resumed = true)
                        } else {
                            next.send(Out.leave())
                            next.close(1000)
                        }
                    }
                    if (myGeneration == generation) reconnecting = null
                    return@launch
                } catch (e: CancellationException) {
                    next.close(1000)
                    throw e
                } catch (e: Exception) {
                    // Any other failure is a failed attempt; nothing may escape this loop and the party's scope.
                    next.close(1000)
                    val code = (e as? PartyException)?.code
                    if (code == PartyException.RESUME_FAILED || code == PartyException.CLIENT_BLOCKED) {
                        if (myGeneration == generation) end(EndReason.LOST, e)
                        return@launch
                    }
                } finally {
                    if (myGeneration == generation) resumeSent = false
                }
            }
        }
    }

    // -- Ending -------------------------------------------------------------------------------------------------------

    /**
     * Leaves the room for good, and the party ends with [EndReason.LEFT].
     *
     * During `create` or `join`, it cancels the call instead. The call fails with `cancelled`, and the party goes back
     * to idle. Outside a room, it does nothing.
     */
    public fun leave() {
        when (connection) {
            Connection.CONNECTED -> {
                link?.send(Out.leave())
                link?.close(1000)
                end(EndReason.LEFT)
            }
            Connection.RECONNECTING -> {
                link?.close(1000)
                end(EndReason.LEFT)
            }
            Connection.CONNECTING -> cancelAttempt()
            Connection.IDLE, Connection.ENDED -> {}
        }
    }

    /** The room's position now in the reference file's timeline, or null without media or a clock sample. */
    public fun roomPositionMs(): Long? {
        val room = room ?: return null
        val media = media ?: return null
        val now = link?.relayNow() ?: return null
        return room.positionAt(media.durationMs, now)
    }

    /**
     * Leaves the room if the party is in one, and releases everything.
     *
     * The adapter is released as [detachPlayer] releases it. The party can't be used again.
     *
     * A `create` or `join` that already reached the relay finishes in the background first, so that the party leaves
     * the room it may have joined.
     *
     * @return a job that completes once no released adapter has a command queued or running, a rate restore included.
     *   It never fails. Join it before disposing of the player.
     */
    public fun close(): Job {
        if (closed) return released()
        val answering = startJob?.takeIf { connection == Connection.CONNECTING && startSent }
        val resuming = reconnecting?.takeIf { connection == Connection.RECONNECTING && resumeSent }
        leave()
        release()
        closed = true
        // Released adapters' lanes run out first, a rate restore included (SDK.md §11.2).
        val done = released()
        val waits = listOfNotNull(answering, resuming)
        if (waits.isEmpty() && done.isCompleted) {
            scope.cancel()
        } else {
            scope.launch {
                waits.joinAll()
                done.join()
                scope.cancel()
            }
        }
        return done
    }

    /** SDK.md §7.4: the end releases the adapter, while the room's rate is still known for a rate restore. */
    private fun end(reason: EndReason, error: PartyException? = null) {
        release()
        link?.close(1000)
        resetSession()
        connection = Connection.ENDED
        endReason = reason
        publish()
        _events.tryEmit(PartyEvent.Ended(reason, error))
    }

    /**
     * Forgets the room and stops every timer; the next session starts over. The adapter stays attached, with its ref:
     * only [detachPlayer], [attachPlayer] with another adapter, the end and [close] release it (SDK.md §7.2, §11.2).
     */
    private fun resetSession() {
        unbind()
        generation++
        startSent = false
        tick?.cancel()
        tick = null
        // A resume attempt that sent `resume` waits for its answer, and a `joined` waiting for its clock leaves now.
        resumeCheck?.invoke()
        if (!resumeSent) reconnecting?.cancel()
        resumeCheck = null
        reconnecting = null
        resumeSent = false
        resumeWindow?.cancel()
        resumeWindow = null
        cancelMatching()
        playTimer?.cancel()
        playTimer = null
        holdTimer?.cancel()
        holdTimer = null
        holdRe = null
        statusTimer?.cancel()
        statusTimer = null
        heartbeat?.cancel()
        heartbeat = null
        link = null
        peerId = null
        resumeMessage = null
        room = null
        epoch = 0
        media = null
        own = null
        ownEpoch = false
        pendingSetMedia = null
        match = null
        choice = null
        mismatch = null
        candidates = emptyList()
        offsetMs = 0
        away = false
        lastSeekAtMs = null
        stepWanted = false
        playerState = PlayerState.IDLE
        lastStatus = null
        lastStatusAtMs = null
        statusForced = false
    }

    private fun publish() {
        _state.value = PartyState(
            connection = connection,
            peerId = peerId,
            room = room,
            media = media,
            match = if (media != null) match else null,
            choice = choice,
            mismatch = mismatch,
            candidates = candidates,
            offsetMs = offsetMs,
            player = playerState,
            endReason = endReason,
        )
    }

    private companion object {
        /** The relay closed this connection because a `resume` elsewhere took the participant over. */
        const val CLOSE_REPLACED = 4000

        /** The completion of a release with nothing left to wait for. */
        val SETTLED: Job = Job().apply { complete() }
    }

    /** [media] as this party last sent it in `setMedia`, and [choice], the app's file for it. */
    private class OwnMedia<T>(val media: Media, val choice: Candidate<T>)
}
