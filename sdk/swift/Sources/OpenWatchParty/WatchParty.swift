/// The error that `create` and `join` throw on a party after `close()` (SDK.md §7.4).
///
/// Calling them after `close()` is a programming error, so this error is not a `PartyError`.
public struct PartyClosedError: Error, Sendable, CustomStringConvertible {
    public var description: String { "the party is closed" }
}

/// One participant's session in a room (SDK.md §16.2).
///
/// A party does this work for the app:
///
/// - It keeps the connection to the room's relay, and the room state.
/// - It matches the room's media to the user's own file through a `Resolver`.
/// - It sends `status` reports.
/// - It drives the attached `PlayerAdapter`, so that the player follows the room.
///
/// The party is isolated to the main actor, and calls the adapter, the resolver and its transport there.
///
/// `Ref` is the app's value for a file, such as a URL or a library item ID. The party compares two refs with `==`
/// (SDK.md §10.2).
@MainActor
public final class WatchParty<Ref: Equatable & Sendable> {
    private let relays: Relays
    private let client: String
    private let name: String
    private let resolver: Resolver<Ref>?
    private let openSocket: WebSocketConnect
    private let clock: any PartyClock
    private var random: any RandomNumberGenerator

    /// Creates a party.
    ///
    /// - Parameters:
    ///   - relays: The relay list the party creates and joins rooms with. An app keeps one for all its parties.
    ///   - client: The app's `<app>/<version>`, such as `Example/1.0`, in 1 to 64 characters. Everyone in the room
    ///     sees it, and the relay's terms use it.
    ///   - name: The user's display name, in 1 to 32 characters.
    ///   - resolver: Finds the user's files for other participants' media. Without a resolver, the party never
    ///     searches. Other participants' media then has no candidates, and the match is unavailable from the start
    ///     (SDK.md §9.2).
    ///   - connect: Opens the party's WebSockets. The default is `urlSessionConnect`.
    ///   - clock: Local time and timers. Tests pass a virtual clock.
    ///   - random: The source of the party's only random draws, which spread out reconnects (SDK.md §14). Each draw u
    ///     is `Double.random(in: 0..<1, using:)`.
    /// - Throws: `PartyError` with code `invalid_message` when `client` or `name` breaks SPEC.md §6.3. That section
    ///   counts lengths in code points, refuses forbidden characters and gives `client` its `<app>/<version>` shape.
    public init(
        relays: Relays,
        client: String,
        name: String,
        resolver: Resolver<Ref>? = nil,
        connect: WebSocketConnect? = nil,
        clock: any PartyClock = SystemClock(),
        random: any RandomNumberGenerator = SystemRandomNumberGenerator()
    ) throws {
        if let problem = nameProblem(name) { throw PartyError(.invalidMessage, "name \(problem)") }
        if let problem = clientProblem(client) { throw PartyError(.invalidMessage, "client \(problem)") }
        self.relays = relays
        self.client = client
        self.name = name
        self.resolver = resolver
        openSocket = connect ?? { try await urlSessionConnect($0) }
        self.clock = clock
        self.random = random
    }

    /// One draw u in [0, 1) from `random` (SDK.md §16.8).
    private func draw() -> Double { Double.random(in: 0..<1, using: &random) }

    /// Everything the UI shows.
    ///
    /// The state changes on events, never per tick. `states()` reports every change.
    public private(set) var state = PartyState<Ref>()

    private var stateSubscribers: [Int: AsyncStream<PartyState<Ref>>.Continuation] = [:]
    private var eventSubscribers: [Int: AsyncStream<any PartyEvent>.Continuation] = [:]
    private var nextSubscriber = 0
    private var streamsFinished = false

    /// A new subscription to `state`.
    ///
    /// The stream yields every state published from now on, in order. It finishes when the party closes.
    public func states() -> AsyncStream<PartyState<Ref>> {
        let (stream, continuation) = AsyncStream<PartyState<Ref>>.makeStream()
        if streamsFinished {
            continuation.finish()
            return stream
        }
        let id = nextSubscriber
        nextSubscriber += 1
        stateSubscribers[id] = continuation
        continuation.onTermination = { [weak self] _ in
            spawn { self?.stateSubscribers[id] = nil }
        }
        return stream
    }

    /// A new subscription to the party's events (SDK.md §15).
    ///
    /// The events report actions, participants coming and going, refusals, resolver failures, a player that refused to
    /// start, and the end of the party.
    ///
    /// The stream never drops an event. It gets every event emitted while it is subscribed, in order, however slowly it
    /// is read. It finishes when the party closes, after `EndedEvent`.
    public func events() -> AsyncStream<any PartyEvent> {
        let (stream, continuation) = AsyncStream<any PartyEvent>.makeStream()
        if streamsFinished {
            continuation.finish()
            return stream
        }
        let id = nextSubscriber
        nextSubscriber += 1
        eventSubscribers[id] = continuation
        continuation.onTermination = { [weak self] _ in
            spawn { self?.eventSubscribers[id] = nil }
        }
        return stream
    }

    // MARK: Session

    private var closed = false
    private var connection = Connection.idle
    private var endReason: EndReason?

    /// Bumped when a session starts or ends: work of an older one is dropped.
    private var generation = 0

    /// The connection in use; while reconnecting, the dropped one, whose clock the steps still use.
    private var link: Link?

    /// The party's place in its room, which a resume takes again (SDK.md §14): set by every `joined`, nil once the
    /// party ends or leaves. Never in `state`, so logging the state never logs the token.
    private var place: Place?
    private var peerId: String? { place?.peerId }
    private var room: Room?

    private var epoch: Int64 = 0
    private var media: Media?
    /// The descriptor of this party's last `setMedia`, and the choice for it: the app's file (SDK.md §9.1).
    private var own: OwnMedia?
    /// The current epoch holds the party's own media, as decided when it started (SDK.md §9.1).
    private var ownEpoch = false
    /// The text of this party's `setMedia` while it is pending (SDK.md §10.2), to send again after a resume (§14).
    private var pendingSetMedia: String?

    private var match: Match?
    private var choice: Candidate<Ref>?
    private var mismatch: Mismatch<Ref>?
    private var candidates: [Ranked<Ref>] = []
    private var matching: Task<Void, Never>?

    /// The probe of the latest `choose` while it runs; cancelling matching cancels it.
    private var manualProbe: ManualProbe?

    /// Bumped by every `choose` and every cancel of matching: a `choose` that started before drops its result.
    private var chooseTurn = 0
    private var offsetMs: Int64 = 0

    private var tick: Alarm?
    private var reconnecting: Task<Void, Never>?
    /// The running resume attempt sent `resume`: it outlives the end of the party until its answer (SDK.md §14).
    private var resumeSent = false

    /// Re-checks the running resume attempt's answer; the end calls it, so a `joined` that waits for a clock leaves.
    private var resumeCheck: ResumeCheck?
    private var resumeWindow: Alarm?

    // MARK: Player

    private var adapter: (any PlayerAdapter)?
    /// The `ref` of the file `adapter` has open; the adapter is bound only while it is the choice's (SDK.md §10.2).
    private var attachedRef: Ref?
    private var away = false
    private var bound = false
    /// The bound adapter refused to start: a `play` the SDK issued threw and it read paused (SDK.md §10.2).
    private var blocked = false
    private var appliedRate = 1.0
    private var nudging = false
    private var lastSeekAtMs: Int64?
    private var playerState = PlayerState.idle
    /// The adapters with a command queued or running, each with its own lane (SDK.md §11.1).
    private var lanes: [Lane] = []
    /// Settles once no released adapter's lane is busy; nil while nobody waits for that.
    private var drained: (waiter: Waiter<Void>, task: Task<Void, Never>)?
    /// A step was asked for while the attached adapter's commands ran; it runs once they are done (SDK.md §11.1).
    private var stepWanted = false
    private var playTimer: Alarm?

    private var holdTimer: Alarm?
    private var holdRe: String?

    // MARK: Status

    private var lastStatus: StatusFields?
    private var lastStatusAtMs: Int64?
    private var statusTimer: Alarm?
    private var statusForced = false
    private var heartbeat: Alarm?

    /// The running `create` or `join`, the way to fail its wait, and whether it sent its message already.
    private var startJob: Task<Void, Never>?
    private var startResult: ((any Error) -> Void)?
    private var startSent = false

    // MARK: Create and join

    /// Creates a room and returns its code (SDK.md §6).
    ///
    /// With several relays, the party connects to all of them at once. It creates the room on the first relay whose
    /// clock gets a sample.
    ///
    /// When a relay answers `relay_full`, the party passes `create` on to the first untried relay of the list.
    ///
    /// - Returns: The room code, as 6 unformatted symbols.
    /// - Throws: `PartyError` when creating fails, with code `relay_full` when every relay is full. Throws
    ///   `PartyClosedError` after `close()`, and `CancellationError` when the calling task is cancelled.
    public func create() async throws -> String {
        try await attempt { [self] myGeneration in
            let list = relays.list
            let message = Out.create(name: name, client: client)
            let first: (link: Link, index: Int)
            if list.count == 1 {
                first = (try await connect(list[0].url), 0)
            } else {
                first = try await race(list)
            }
            var link = first.link
            var tried: Set = [first.index]
            while true {
                do {
                    try await admitNew(link, message, "create", myGeneration)
                    return place!.code
                } catch let error as PartyError where error.code == .relayFull {
                    guard let next = list.indices.first(where: { !tried.contains($0) }) else { throw error }
                    tried.insert(next)
                    link = try await connect(list[next].url)
                }
            }
        }
    }

    /// Joins the room `code` on the relay that owns it (SDK.md §4).
    ///
    /// `code` may be typed or pasted, so `k7m-q2x` works too. The party checks the code first, so an invalid code fails
    /// with `invalid_code` rather than `already_in_room`.
    ///
    /// With `create`, the relay makes a room with this code when no room has it, and the party becomes that room's host
    /// (SPEC.md §7.2). Use it to bring a group back to a code it used before, such as one from a list of recent rooms. A
    /// relay that predates `create` answers `room_not_found`.
    ///
    /// - Parameters:
    ///   - code: The room code, typed or pasted.
    ///   - create: Whether the relay makes the room when no room has the code.
    /// - Throws: `PartyError` when joining fails, with code `relay_full` when `create` finds the relay full. Throws
    ///   `PartyClosedError` after `close()`, and `CancellationError` when the calling task is cancelled.
    public func join(_ code: String, create: Bool = false) async throws {
        try requireOpen()
        guard let normalized = normalizeCode(code) else { throw PartyError(.invalidCode, "not a room code") }
        let message = Out.join(code: normalized, name: name, client: client, create: create)
        try await attempt { [self] myGeneration in
            var refreshed = false
            var found = route(normalized, relays.list)
            if found == nil {
                await relays.refresh()
                refreshed = true
                found = route(normalized, relays.list)
            }
            guard let relay = found else { throw PartyError(.roomNotFound) }
            do {
                try await joinAt(relay, message, myGeneration)
            } catch let error as PartyError where error.code == .roomNotFound && !refreshed {
                await relays.refresh()
                guard let moved = route(normalized, relays.list), moved.url != relay.url else { throw error }
                try await joinAt(moved, message, myGeneration)
            }
        }
    }

    private func joinAt(_ relay: Relay, _ message: String, _ myGeneration: Int) async throws {
        let link = try await connect(relay.url)
        try await admitNew(link, message, "join", myGeneration)
    }

    /// Runs a `create` or `join` as `startJob` and waits for it. `leave()` and `close()` fail the wait at once with
    /// `cancelled` (SDK.md §7.2); cancelling the calling task fails it with `CancellationError` and cancels the attempt
    /// likewise.
    private func attempt<R>(_ block: @escaping @MainActor (_ myGeneration: Int) async throws -> R) async throws -> R {
        let myGeneration = try begin()
        let result = Waiter<Result<R, any Error>>()
        startResult = { result.resume(.failure($0)) }
        startJob = spawn { [self] in
            do {
                result.resume(.success(try await block(myGeneration)))
            } catch {
                result.resume(.failure(failed(myGeneration, error)))
            }
        }
        let outcome = await withTaskCancellationHandler {
            await result.wait { {} }
        } onCancel: {
            spawn { [self] in
                result.resume(.failure(CancellationError()))
                if myGeneration == generation && connection == .connecting { cancelAttempt() }
            }
        }
        return try outcome.get()
    }

    /// `leave()` or `close()` during `create` or `join`: back to idle without an event; the attempt stops, except that
    /// a connection that sent its message waits for the answer and leaves a room it joined.
    private func cancelAttempt() {
        let job = startJob
        let result = startResult
        if !startSent { job?.cancel() }
        resetSession()
        connection = .idle
        publish()
        result?(PartyError(.cancelled, "left while connecting"))
    }

    private func requireOpen() throws {
        if closed { throw PartyClosedError() }
    }

    private func requireStartable() throws {
        try requireOpen()
        if connection == .connecting || connection == .connected || connection == .reconnecting {
            throw PartyError(.alreadyInRoom, "the party is in a room or connecting")
        }
    }

    /// Starts over for a new `create` or `join`: away is cleared, and an attached adapter stays attached into the new
    /// session (SDK.md §7.2).
    private func begin() throws -> Int {
        try requireStartable()
        resetSession()
        connection = .connecting
        endReason = nil
        publish()
        return generation
    }

    /// A failed `create` or `join`: back to idle when the attempt is still current, keeping the adapter, and the error
    /// to report.
    private func failed(_ myGeneration: Int, _ error: any Error) -> any Error {
        if myGeneration == generation && connection == .connecting {
            link?.close()
            resetSession()
            connection = .idle
            publish()
        }
        switch error {
        case is PartyError, is PartyClosedError: return error
        case is CancellationError: return PartyError(.cancelled, "cancelled")
        default: return PartyError(.unreachable, "connection failed", underlying: error)
        }
    }

    /// Opens a connection to `url` within the connect timeout.
    private func connect(_ url: String) async throws -> Link {
        let open = openSocket
        let outcome = await clock.race(timeoutMs: Owp.connectTimeoutMs, late: { $0.close(code: 1000) }) {
            try await open(url)
        }
        switch outcome {
        case .value(let socket):
            let link = Link(url: url, socket: socket, clock: clock)
            link.start()
            return link
        case .failure(let error):
            if error is CancellationError && Task.isCancelled { throw error }
            throw PartyError(.unreachable, "could not connect to \(url)", underlying: error)
        case .timedOut:
            throw PartyError(.unreachable, "connecting to \(url) timed out")
        }
    }

    /// Connects to every relay of `list` at once, each connection with its own clock (SDK.md §6). The first one whose
    /// clock gets a sample wins, and every other connection and attempt is closed with 1000. Fails with `unreachable`
    /// when none gets a sample within the connect timeout or every one failed first.
    private func race(_ list: [Relay]) async throws -> (link: Link, index: Int) {
        let race = Race(count: list.count)
        let open = openSocket
        let clock = clock
        let opening = list.indices.map { i in
            spawn {
                guard let socket = try? await open(list[i].url) else {
                    race.failed()
                    return
                }
                if race.winner.isSettled || Task.isCancelled {
                    socket.close(code: 1000)
                    return
                }
                let link = Link(url: list[i].url, socket: socket, clock: clock)
                link.onPong = { [weak link] in if link?.sync.best != nil { race.winner.resume(i) } }
                link.onClosed = { _ in race.failed() }
                race.links[i] = link
                link.start()
            }
        }
        let won = await clock.wait(race.winner, timeoutMs: Owp.connectTimeoutMs, timedOut: nil, cancelled: nil)
        for task in opening { task.cancel() }
        let winner = won.flatMap { Task.isCancelled ? nil : race.links[$0] }
        for link in race.links where link !== winner { link?.close(1000) }
        try Task.checkCancellation()
        guard let won, let winner else { throw PartyError(.unreachable, "no relay answered in time") }
        winner.onPong = {}
        winner.onClosed = { _ in }
        return (winner, won)
    }

    /// Sends `text` (`create`, `join` or `resume`) and waits up to the connect timeout for `joined` or a refusal of
    /// `type`. While `needClock` returns true, also for the connection's first clock sample; `resumeCheck` re-checks
    /// that wait. `admitted` runs once both are in, before any later message of `link` is handled.
    private func admit(
        _ link: Link,
        _ text: String,
        _ type: String,
        needClock: (() -> Bool)?,
        admitted: @escaping (In.Joined) -> Void
    ) async throws {
        if !link.isOpen { throw PartyError(.unreachable, "connection closed") }
        let done = Waiter<Admission>()
        var joined: In.Joined?
        var later: [In] = []
        func finish() {
            guard let j = joined, !done.isSettled else { return }
            if needClock?() == true && link.sync.best == nil { return }
            admitted(j)
            done.resume(.joined)
            for message in later { handle(link, message) }
        }
        link.onMessage = { [self] message in
            if done.isSettled {
                handle(link, message)
                return
            }
            if joined != nil {
                later.append(message)
                return
            }
            switch message {
            case .joined(let j):
                joined = j
                finish()
            case .malformedJoined:
                // A relay fault (SDK.md §8): leave a room it may have made us a member of; a resume just fails.
                if type != "resume" { link.send(Out.leave()) }
                done.resume(.failed(PartyError(.invalidMessage, "the relay sent a malformed joined")))
            case .error(let code, let message, let re) where re == type:
                done.resume(.failed(PartyError(PartyError.Code(rawValue: code), message, re: re)))
            default:
                break
            }
        }
        link.onPong = { finish() }
        link.onClosed = { code in
            done.resume(.failed(PartyError(.unreachable, "connection closed (\(code.map { "\($0)" } ?? "no status"))")))
        }
        let check = ResumeCheck(finish)
        if needClock != nil { resumeCheck = check }
        link.send(text)
        let outcome = await clock.wait(done, timeoutMs: Owp.connectTimeoutMs, timedOut: .timedOut, cancelled: .cancelled)
        if resumeCheck === check { resumeCheck = nil }
        switch outcome {
        case .joined:
            return
        case .failed(let error):
            throw error
        case .timedOut:
            link.close()
            throw PartyError(.unreachable, "no answer to \(type)")
        case .cancelled:
            throw CancellationError()
        }
    }

    /// `create` or `join` on `link`; on `joined` the party becomes a member. Once the attempt is cancelled, a `joined`
    /// leaves at once.
    private func admitNew(_ link: Link, _ text: String, _ type: String, _ myGeneration: Int) async throws {
        if myGeneration != generation {
            link.close(1000)
            throw PartyError(.cancelled, "left while connecting")
        }
        self.link = link
        startSent = true
        defer { if myGeneration == generation { startSent = false } }
        do {
            try await admit(link, text, type, needClock: nil) { [self] joined in
                if myGeneration != generation {
                    // Cancelled while the relay answered: don't stay behind as a disconnected participant.
                    link.send(Out.leave())
                    link.close(1000)
                } else {
                    joinedOn(link, joined, resumed: false)
                }
            }
        } catch {
            link.close(1000)
            if myGeneration != generation { throw PartyError(.cancelled, "left while connecting") }
            throw error
        }
        if myGeneration != generation { throw PartyError(.cancelled, "left while connecting") }
    }

    /// SDK.md §7.3: `joined` on a new connection, after `create`, `join` or a resume.
    private func joinedOn(_ link: Link, _ joined: In.Joined, resumed: Bool) {
        let previous = room
        self.link = link
        place = Place(url: link.url, code: joined.code, peerId: joined.peerId, token: joined.token)
        link.onMessage = { [self] in handle(link, $0) }
        link.onPong = {}
        link.onClosed = { [self] in linkClosed(link, $0) }
        room = joined.room
        connection = .connected
        resumeWindow?.cancel()
        resumeWindow = nil
        if resumed, let previous { participantEvents(previous, joined.room) }
        if !resumed || joined.room.epoch != epoch {
            pendingSetMedia = nil
            newEpoch(joined.room.epoch, joined.media)
        }
        // A `setMedia` still pending never reached the room: as host the party sends it again after the status, else
        // it gives it up (SDK.md §14).
        let isHost = joined.room.participants.contains { $0.peerId == joined.peerId && $0.host }
        let resend = isHost ? pendingSetMedia : nil
        if resend == nil && pendingSetMedia != nil {
            pendingSetMedia = nil
            rebind()
        }
        lastStatus = nil
        lastStatusAtMs = nil
        statusTimer?.cancel()
        statusTimer = nil
        if tick == nil { tick = clock.every(Owp.tickMs) { [self] in onTick() } }
        updatePlayer()
        publish()
        requestStatus(force: true)
        if let resend { link.send(resend) }
        step()
        // Every `joined` may refresh the relay list in the background (SDK.md §3.3).
        let relays = relays
        spawn { await relays.refreshInBackground() }
    }

    // MARK: Relay messages

    private func handle(_ link: Link, _ message: In) {
        guard link === self.link, connection == .connected else { return }
        switch message {
        case .room(let next, let action, let mediaJSON):
            snapshot(next, action, mediaJSON)
        case .error(let code, let message, let re):
            refused(PartyError(PartyError.Code(rawValue: code), message, re: re))
        case .closed(let reason):
            link.close(1000)
            endParty(reason == "kicked" ? .kicked : reason == "expired" ? .expired : .ended)
        case .joined, .malformedJoined, .pong:
            break
        }
    }

    /// An accepted `room` (SDK.md §8). One of a new epoch must carry that epoch's media, else the party can't follow the
    /// room; in order, the snapshot replaces the old one and the new epoch starts, then come its events and a step, as
    /// for `joined`. A `media` member of the party's own epoch is ignored.
    private func snapshot(_ next: Room, _ action: In.Action?, _ mediaJSON: JSON?) {
        guard let previous = room, next.rev > previous.rev else { return }
        var newMedia: Media??
        if next.epoch != epoch {
            guard let media = mediaJSON.flatMap(In.descriptor) else {
                link?.send(Out.leave())
                endParty(.lost, PartyError(.invalidMessage, "the relay sent a new epoch without valid media"))
                return
            }
            newMedia = media
        }
        room = next
        if let newMedia {
            pendingSetMedia = nil
            newEpoch(next.epoch, newMedia)
        }
        participantEvents(previous, next)
        if let action {
            emit(ActionEvent(
                kind: action.kind,
                byPeerId: action.by,
                by: action.by.flatMap { by in next.participants.first { $0.peerId == by } },
                byMe: action.by != nil && action.by == peerId,
                room: next
            ))
            endHold()
        }
        updatePlayer()
        publish()
        requestStatus()
        step()
    }

    private func participantEvents(_ previous: Room, _ next: Room) {
        let before = Set(previous.participants.map(\.peerId))
        let after = Set(next.participants.map(\.peerId))
        for p in next.participants where !before.contains(p.peerId) { emit(ParticipantJoinedEvent(participant: p)) }
        for p in previous.participants where !after.contains(p.peerId) { emit(ParticipantLeftEvent(participant: p)) }
    }

    private func refused(_ error: PartyError) {
        if error.re != nil && error.re == holdRe { endHold() }
        if error.re == "setMedia" && pendingSetMedia != nil {
            // The binding rule, then a step (SDK.md §10.2).
            pendingSetMedia = nil
            rebind()
            updatePlayer()
            publish()
            requestStatus()
            step()
        }
        if error.re != "create" && error.re != "join" && error.re != "resume" { emit(ErrorEvent(error: error)) }
    }

    // MARK: Media and matching (SDK.md §9)

    private func newEpoch(_ newEpoch: Int64, _ newMedia: Media?) {
        epoch = newEpoch
        media = newMedia
        cancelMatching()
        offsetMs = 0
        choice = nil
        mismatch = nil
        candidates = []
        unbind()
        ownEpoch = newMedia.map { m in own.map { m.sameDescriptor(as: $0.media) } ?? false } ?? false
        if let newMedia {
            if ownEpoch {
                match = Match(.matched, deltaMs: 0)
                choice = own?.choice
            } else {
                if let adapter { command(adapter) { try await $0.pause() } }
                match = Match(.searching)
                startMatching(newMedia)
            }
        } else {
            match = nil
            if let adapter { command(adapter) { try await $0.pause() } }
        }
        rebind()
    }

    private func cancelMatching() {
        matching?.cancel()
        matching = nil
        chooseTurn += 1
        manualProbe?.cancel()
        manualProbe = nil
    }

    /// The resolver and the walk for `target` (SDK.md §9.2, §9.5), then the outcome (§9.6). Without a resolver there
    /// are no candidates: `match` is unavailable at once.
    private func startMatching(_ target: Media) {
        guard let resolver else {
            match = Match(.unavailable)
            return
        }
        matching = spawn { [self] in
            guard let found = try? await resolve(target, with: resolver),
                  let result = try? await walk(media: target, candidates: found, clock: clock, progress: { [self] in
                      candidates = $0
                      publish()
                  }) else { return }
            switch result.outcome {
            case .matched(let candidate, let deltaMs):
                choice = candidate
                match = Match(.matched, deltaMs: deltaMs)
            case .mismatch(let candidate, let deltaMs):
                mismatch = Mismatch(candidate: candidate, deltaMs: deltaMs)
                match = Match(.searching, deltaMs: deltaMs)
            case .unavailable:
                match = Match(.unavailable)
            }
            matching = nil
            rebind()
            updatePlayer()
            publish()
            requestStatus()
        }
    }

    /// `resolveAgain()` while a file is chosen (SDK.md §9.1): only the resolver and the ranking, whose result replaces
    /// the candidates, each with a delta from its known duration; no probes, and nothing else changes. Without a
    /// resolver the candidates are empty at once.
    private func startRanking(_ target: Media) {
        guard let resolver else {
            candidates = []
            return
        }
        matching = spawn { [self] in
            guard let found = try? await resolve(target, with: resolver) else { return }
            candidates = rank(target, found).map { c in
                Ranked(candidate: c, deltaMs: countedDuration(c.durationMs).map { $0 - target.durationMs })
            }
            matching = nil
            publish()
        }
    }

    /// The resolver's candidates for `target`; none when it throws or takes longer than 30 s, which
    /// `ResolveFailedEvent` reports. Throws `CancellationError` only when matching was cancelled.
    private func resolve(_ target: Media, with resolver: Resolver<Ref>) async throws -> [Candidate<Ref>] {
        let outcome = await clock.race(timeoutMs: Owp.resolveTimeoutMs) { try await resolver.resolve(target) }
        // The timeout, and a cancellation of the resolver's own such as its own timeout, are failures; only matching
        // being cancelled stops the search without an event (SDK.md §9.2).
        try Task.checkCancellation()
        switch outcome {
        case .value(let found):
            return found
        case .failure(let error):
            emit(ResolveFailedEvent(error: error))
        case .timedOut:
            emit(ResolveFailedEvent(error: TimeoutError(timeoutMs: Owp.resolveTimeoutMs)))
        }
        return []
    }

    /// Plays the offered file despite the mismatch (SDK.md §9.6).
    ///
    /// `match` becomes mismatched.
    public func acceptMismatch() {
        guard let offered = mismatch else { return }
        choice = offered.candidate
        match = Match(.mismatched, deltaMs: offered.deltaMs)
        mismatch = nil
        rebind()
        updatePlayer()
        publish()
        requestStatus()
    }

    /// Declines the offered file.
    ///
    /// The party doesn't play the file. `match` becomes unavailable, and the room stops waiting for this user.
    public func declineMismatch() {
        guard let offered = mismatch else { return }
        match = Match(.unavailable, deltaMs: offered.deltaMs)
        mismatch = nil
        choice = nil
        rebind()
        updatePlayer()
        publish()
        requestStatus()
    }

    /// Takes a file that the user picked by hand (SDK.md §9.6).
    ///
    /// When the file has no duration, the party probes it first. `candidate` then becomes the choice, also when it has
    /// the current choice's id, so a refreshed `ref` replaces the old one.
    ///
    /// The latest call wins. A call returns false, and nothing changes, in these cases:
    ///
    /// - The room has no media.
    /// - The file has no duration from 1 ms to 7 days, and no probe.
    /// - The probe fails.
    /// - A later call started while this call was still probing, and cancelled it.
    /// - Matching was cancelled, by a new epoch, `resolveAgain()` or the end.
    /// - The calling task was cancelled.
    ///
    /// - Returns: True once `candidate` is the choice, or false in the cases above.
    public func choose(_ candidate: Candidate<Ref>) async -> Bool {
        guard let target = media else { return false }
        manualProbe?.cancel()
        chooseTurn += 1
        let myTurn = chooseTurn
        var duration = countedDuration(candidate.durationMs)
        if duration == nil, let probe = candidate.probe { duration = countedDuration(await probeByHand(probe)) }
        guard let duration, myTurn == chooseTurn, media != nil else { return false }
        cancelMatching()
        let delta = duration - target.durationMs
        choice = candidate
        mismatch = nil
        match = Match(abs(delta) <= Owp.matchToleranceMs ? .matched : .mismatched, deltaMs: delta)
        rebind()
        updatePlayer()
        publish()
        requestStatus()
        return true
    }

    /// Runs `probe` for `choose` as `manualProbe`; nil when it fails, the SDK cancels it or the calling task is
    /// cancelled. A throw, a cancellation of the probe's own included, is no duration (SDK.md §9.2).
    private func probeByHand(_ probe: @escaping @MainActor @Sendable () async throws -> Int64?) async -> Int64? {
        let waiter = Waiter<Int64?>()
        let task = spawn {
            let duration = try? await probe()
            waiter.resume(duration ?? nil)
        }
        let handle = ManualProbe {
            task.cancel()
            waiter.resume(nil)
        }
        manualProbe = handle
        let duration = await withTaskCancellationHandler {
            await waiter.wait { {} }
        } onCancel: {
            task.cancel()
            spawn { waiter.resume(nil) }
        }
        task.cancel()
        if manualProbe === handle { manualProbe = nil }
        return duration
    }

    /// Asks the resolver again for the current media (SDK.md §9.1).
    ///
    /// The call keeps what plays, so the offset and the adapter stay as they are.
    ///
    /// - Without a choice, matching starts over with the resolver and the walk.
    /// - With a choice, only the resolver and the ranking run. Their result replaces `state.candidates` for `choose`,
    ///   and the choice and the match stay.
    ///
    /// The call does nothing without media, or in an epoch of the party's own media.
    public func resolveAgain() {
        guard let target = media, !ownEpoch else { return }
        cancelMatching()
        if choice == nil {
            match = Match(.searching)
            mismatch = nil
            candidates = []
            startMatching(target)
        } else {
            startRanking(target)
        }
        updatePlayer()
        publish()
        requestStatus()
    }

    // MARK: The player (SDK.md §10.2, §11)

    /// Tells the party that the user's player `player` has the file `ref` open.
    ///
    /// `ref` is normally `state.choice.ref` as published. As host, it can also be the `ref` passed to `setMedia`.
    ///
    /// The party follows the room on the player, which binds it, whenever all of these hold (SDK.md §10.2):
    ///
    /// - `ref` equals the choice's `ref`.
    /// - The match is matched or mismatched.
    /// - No `setMedia` of this party is pending.
    ///
    /// The party re-checks these conditions on every change, so attach once per file the player opens. A file opened
    /// for a choice that has since changed stays unbound.
    ///
    /// Attaching another adapter releases the one attached before, as `detachPlayer` does. Attaching the same adapter
    /// again replaces its ref and keeps its queued commands.
    ///
    /// The call works while the party is idle or ended too. The adapter stays attached into the session that the next
    /// `create`, `join` or `resume` starts, also when an attempt fails.
    ///
    /// The party's end releases the adapter. An app whose player still has its file open after `EndedEvent` therefore
    /// attaches it again.
    public func attachPlayer(_ player: any PlayerAdapter, ref: Ref) {
        if player !== adapter {
            release()
            lastSeekAtMs = nil
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

    /// Tells the party that the user left the player, so the room stops waiting for this user.
    ///
    /// Without `player`, the call releases the attached adapter. With `player`, it does so only when `player` is the
    /// one attached. Otherwise nothing changes.
    ///
    /// The party drops a released adapter's commands that haven't started. If the SDK was nudging the adapter, the
    /// adapter gets the room's rate back once (SDK.md §11.2).
    ///
    /// - Returns: A task that completes once no released adapter has a command queued or running. Await it before
    ///   disposing of the player.
    @discardableResult
    public func detachPlayer(_ player: (any PlayerAdapter)? = nil) -> Task<Void, Never> {
        if player == nil || player === adapter {
            release()
            away = true
            updatePlayer()
            publish()
            requestStatus()
        }
        return released()
    }

    /// Tells the party that the player's state changed.
    ///
    /// Calling it is optional. Without the call, the SDK sees the change on the next tick.
    public func playerChanged() {
        updatePlayer()
        publish()
        requestStatus()
        step()
    }

    /// Whether the attached adapter plays the current choice (SDK.md §10.2): its ref is the choice's, the match is
    /// matched or mismatched, and no `setMedia` of this party is pending.
    private func bindable() -> Bool {
        guard adapter != nil, media != nil, pendingSetMedia == nil, let chosen = choice else { return false }
        let state = match?.state
        return chosen.ref == attachedRef && (state == .matched || state == .mismatched)
    }

    /// Binds or unbinds as `bindable` says now; called after every change of what it reads.
    private func rebind() {
        let want = bindable()
        if want == bound { return }
        if want { bind() } else { unbind() }
    }

    /// Binding (SDK.md §10.2): no nudging, the room's rate once, then a step.
    private func bind() {
        guard let player = adapter else { return }
        bound = true
        nudging = false
        let rate = room?.rate ?? 1
        appliedRate = rate
        if player.canSetRate { command(player) { try await $0.setRate(rate) } }
        step()
    }

    /// Unbinding never touches the adapter's queued commands; it ends blocked (SDK.md §10.2).
    private func unbind() {
        bound = false
        blocked = false
        nudging = false
        playTimer?.cancel()
        playTimer = nil
    }

    /// Releases the attached adapter (SDK.md §11.2): its commands that haven't started are dropped, and while nudging
    /// it gets the room's rate back once, as the rate is now. Its lane runs out on its own; nothing else reaches it.
    private func release() {
        guard let player = adapter else { return }
        lane(player)?.queue.removeAll { !$0.restore }
        if nudging {
            let rate = room?.rate ?? 1
            command(player, restore: true) { try await $0.setRate(rate) }
        }
        unbind()
        adapter = nil
        attachedRef = nil
        stepWanted = false
    }

    private func lane(_ player: any PlayerAdapter) -> Lane? { lanes.first { $0.player === player } }

    /// Queues a command on `player`'s lane. A failed command is ignored: the next step undoes it. Once the attached
    /// adapter's lane is empty, the party reads the player again and runs a step asked for meanwhile.
    private func command(
        _ player: any PlayerAdapter,
        seek: Bool = false,
        restore: Bool = false,
        _ run: @escaping @MainActor (any PlayerAdapter) async throws -> Void
    ) {
        let command = Command(seek: seek, restore: restore, run: run)
        if let lane = lane(player) {
            lane.queue.append(command)
            return
        }
        // In `lanes` before the runner starts, so it counts as busy from now on.
        let lane = Lane(player: player)
        lane.queue.append(command)
        lanes.append(lane)
        spawn { [self] in
            while !lane.queue.isEmpty {
                let next = lane.queue.removeFirst()
                lane.running = next
                // A failed command is undone by the next step.
                try? await next.run(player)
                lane.running = nil
            }
            lanes.removeAll { $0 === lane }
            checkDrained()
            guard player === adapter else { return }
            updatePlayer()
            publish()
            requestStatus()
            if stepWanted {
                stepWanted = false
                step()
            }
        }
    }

    /// Completes once no lane but the attached adapter's is busy (SDK.md §11.2); at once when none is.
    private func released() -> Task<Void, Never> {
        if lanes.allSatisfy({ $0.player === adapter }) { return spawn {} }
        if let drained { return drained.task }
        let waiter = Waiter<Void>()
        let task = spawn { await waiter.wait { {} } }
        drained = (waiter, task)
        return task
    }

    private func checkDrained() {
        guard let waiting = drained, lanes.allSatisfy({ $0.player === adapter }) else { return }
        drained = nil
        waiting.waiter.resume(())
    }

    private func seekCommand(_ player: any PlayerAdapter, _ positionMs: Int64) {
        lastSeekAtMs = clock.nowMs()
        command(player, seek: true) { try await $0.seek(to: positionMs) }
    }

    /// A seek the SDK issued on `player` is queued or running.
    private func seeking(_ player: any PlayerAdapter) -> Bool {
        guard let lane = lane(player) else { return false }
        return lane.running?.seek == true || lane.queue.contains { $0.seek }
    }

    private func onTick() {
        updatePlayer()
        publish()
        requestStatus()
        step()
    }

    /// The adapter's seek accuracy (SDK.md §11.2); a negative one counts as 0.
    private func accuracyMs(_ player: any PlayerAdapter) -> Int64 { max(0, player.seekAccuracyMs) }

    /// One reconciliation step (SDK.md §11), when the adapter is bound and the room's media and clock are known.
    private func step() {
        guard let player = adapter, let room, let media, bound, holdRe == nil,
              let now = link?.relayNow() else { return }
        if lane(player) != nil {
            stepWanted = true
            return
        }
        let out = reconcile(StepInput(
            phase: room.phase,
            positionMs: room.positionMs,
            atMs: room.atMs,
            rate: room.rate,
            durationMs: media.durationMs,
            nowMs: now,
            offsetMs: offsetMs,
            playerPositionMs: player.positionMs,
            paused: player.paused,
            buffering: player.buffering,
            playerDurationMs: player.durationMs,
            canSetRate: player.canSetRate,
            seekAccuracyMs: accuracyMs(player),
            appliedRate: appliedRate,
            nudging: nudging,
            sinceSeekMs: lastSeekAtMs.map { clock.nowMs() - $0 }
        ))
        nudging = out.nudging
        playTimer?.cancel()
        playTimer = out.playInMs.map { clock.after($0) { [self] in step() } }
        for action in out.actions {
            switch action {
            case .pause:
                command(player) { try await $0.pause() }
            case .setRate(let rate):
                appliedRate = rate
                command(player) { try await $0.setRate(rate) }
            case .seek(let positionMs):
                seekCommand(player, positionMs)
            case .play:
                command(player) { [self] p in
                    do {
                        try await p.play()
                    } catch {
                        refusedToPlay(p, error)
                    }
                }
            }
        }
        // A seek the step queued makes the player loading until it has run (SDK.md §10.2 rule 5).
        updatePlayer()
        publish()
        requestStatus()
    }

    /// A `play` the SDK issued threw: the player refused to start when it still reads paused (SDK.md §10.2).
    private func refusedToPlay(_ player: any PlayerAdapter, _ error: any Error) {
        guard !blocked, player === adapter, bound, player.paused, room?.phase == .playing else { return }
        blocked = true
        updatePlayer()
        publish()
        requestStatus()
        emit(PlayBlockedEvent(error: error))
    }

    /// The player state of SDK.md §10.2.
    private func computePlayerState() -> PlayerState {
        if media == nil || match?.state == .unavailable || away { return .idle }
        guard bound, let player = adapter, let room else { return .loading }
        if !player.firstFrameShown || seeking(player) { return .loading }
        let now = link?.relayNow()
        // Without relay time the SDK can't place the player in a playing room (SDK.md §10.2).
        if room.phase == .playing && now == nil { return .loading }
        var held = room.phase != .playing
        let anchor = room.positionMs + offsetMs
        if !held, let now {
            held = Double(now) < localStartAt(atMs: room.atMs, anchor: anchor, rate: room.rate)
        }
        if held {
            let end = player.durationMs.flatMap { $0 > 0 ? max(0, $0 - Owp.endMarginMs) : nil }
            let holdAt = end.map { min(max(0, anchor), $0) } ?? max(0, anchor)
            if abs(player.positionMs - holdAt) > max(Owp.heldSeekMs, accuracyMs(player)) { return .loading }
        }
        if blocked { return .loading }
        if player.buffering {
            let running = now.map { roomRunning(phase: room.phase, atMs: room.atMs, t: $0) } ?? false
            return running && !held ? .buffering : .loading
        }
        return .ready
    }

    private func updatePlayer() {
        // Blocked ends once the player plays or the room stops playing (SDK.md §10.2).
        if blocked && (adapter?.paused != true || room?.phase != .playing) { blocked = false }
        playerState = connection == .connected || connection == .reconnecting ? computePlayerState() : .idle
    }

    // MARK: Status (SDK.md §10)

    private func statusFields() -> StatusFields {
        StatusFields(
            epoch: epoch, player: playerState, match: media != nil ? match : nil, offsetMs: offsetMs,
            rttMs: link?.sync.best?.rttMs
        )
    }

    /// Sends `status` when it changed (or `force`), at most one per 250 ms except the first for a new epoch, which
    /// goes out at once; never while reconnecting.
    private func requestStatus(force: Bool = false) {
        guard connection == .connected else { return }
        let fields = statusFields()
        let last = lastStatus
        var a = last
        a?.rttMs = nil
        var b = fields
        b.rttMs = nil
        let changed = a != b
        if !changed && !force && !statusForced { return }
        let since = lastStatusAtMs.map { clock.nowMs() - $0 }
        if let since, since < Owp.statusSpacingMs, last?.epoch == fields.epoch {
            if force { statusForced = true }
            if statusTimer == nil {
                statusTimer = clock.after(Owp.statusSpacingMs - since) { [self] in
                    statusTimer = nil
                    requestStatus()
                }
            }
        } else {
            sendStatus(fields)
        }
    }

    private func sendStatus(_ fields: StatusFields) {
        guard let link else { return }
        link.send(Out.status(fields))
        lastStatus = fields
        lastStatusAtMs = clock.nowMs()
        statusForced = false
        statusTimer?.cancel()
        statusTimer = nil
        heartbeat?.cancel()
        heartbeat = clock.after(Owp.statusHeartbeatMs) { [self] in requestStatus(force: true) }
    }

    // MARK: Local commands (SDK.md §12)

    /// Asks the room to play.
    ///
    /// The room starts everyone together.
    public func play() throws {
        try requireController()
        send(Out.play())
    }

    /// Pauses the room.
    ///
    /// The bound player pauses at once, and the party sends its position with the pause, in the reference timeline.
    public func pause() throws {
        try requireController()
        let player = bound ? adapter : nil
        let position = try player.map { try wirePosition($0.positionMs) }
        if let player { command(player) { try await $0.pause() } }
        hold("pause")
        send(Out.pause(positionMs: position))
    }

    /// Seeks the room to `positionMs`.
    ///
    /// `positionMs` is a position in the user's own file.
    public func seek(to positionMs: Int64) throws {
        try requireController()
        if positionMs < 0 { throw PartyError(.invalidMessage, "positionMs is negative") }
        let roomPositionMs = try wirePosition(positionMs)
        let seeks = bound && adapter != nil
        if seeks, let player = adapter { seekCommand(player, positionMs) }
        hold("seek")
        send(Out.seek(positionMs: roomPositionMs))
        // The queued local seek is an SDK seek: the player is loading until it has run (SDK.md §10.2 rule 5, §12).
        if seeks {
            updatePlayer()
            publish()
            requestStatus()
        }
    }

    /// max(0, `positionMs` − offset): a local position as `pause` and `seek` send it (SDK.md §12); throws when the
    /// difference isn't an int.
    private func wirePosition(_ positionMs: Int64) throws -> Int64 {
        guard Owp.intRange.contains(positionMs), Owp.intRange.contains(positionMs - offsetMs) else {
            throw PartyError(.invalidMessage, "the position − offset is beyond 2^53 − 1")
        }
        return max(0, positionMs - offsetMs)
    }

    /// Sets the room's rate, from 0.5 to 2.0.
    public func setRate(_ rate: Double) throws {
        try requireController()
        guard rate >= Owp.minRate && rate <= Owp.maxRate else { throw PartyError(.invalidMessage, "rate is outside 0.5–2.0") }
        if bound, let player = adapter, player.canSetRate {
            appliedRate = rate
            command(player) { try await $0.setRate(rate) }
        }
        hold("rate")
        send(Out.rate(rate))
    }

    /// Sets the offset for a file that differs from the reference (SDK.md §13).
    ///
    /// The local position is the room position plus `offsetMs`. The offset is at most 604800000 ms, which is 7 days,
    /// either way.
    ///
    /// - Throws: `PartyError` with code `invalid_message` for any other value, and nothing changes.
    public func setOffset(_ offsetMs: Int64) throws {
        guard (-Owp.maxOffsetMs...Owp.maxOffsetMs).contains(offsetMs) else {
            throw PartyError(.invalidMessage, "offsetMs is beyond \(Owp.maxOffsetMs) either way")
        }
        self.offsetMs = offsetMs
        updatePlayer()
        publish()
        requestStatus()
        step()
    }

    /// Shares `media` with the room, starting at `positionMs` of the host's own file.
    ///
    /// Only the host shares media. `ref` is the app's value that reopens the host's file.
    ///
    /// Whenever the room plays `media`, `state.choice` is a candidate with id `""`, `ref` and the descriptor's duration
    /// (SDK.md §9.1). This holds also after this party hands over host. The app opens that choice like any other, and a
    /// player already attached with `ref` binds without reopening.
    ///
    /// - Throws: `PartyError` with code `not_in_room` or `forbidden`. It throws `invalid_message` when `checkMedia`
    ///   refuses `media`, or when `positionMs` is negative or beyond 2^53 − 1.
    public func setMedia(_ media: Media, ref: Ref, positionMs: Int64 = 0) throws {
        try requireMember()
        try requireHost()
        if let problem = checkMedia(media) { throw PartyError(.invalidMessage, problem) }
        guard (0...Owp.maxInt).contains(positionMs) else {
            throw PartyError(.invalidMessage, "positionMs is negative or beyond 2^53 − 1")
        }
        own = OwnMedia(media: media, choice: Candidate(id: "", ref: ref, durationMs: media.durationMs))
        let text = Out.setMedia(media, positionMs: positionMs)
        pendingSetMedia = text
        rebind()
        updatePlayer()
        publish()
        send(text)
        requestStatus()
    }

    public func clearMedia() throws {
        try requireMember()
        try requireHost()
        send(Out.clearMedia())
    }

    public func setControl(_ mode: Control) throws {
        try requireMember()
        try requireHost()
        send(Out.setControl(mode))
    }

    public func transferHost(_ peerId: String) throws {
        try requireMember()
        try requireHost()
        try requireParticipant(peerId)
        send(Out.transferHost(peerId))
    }

    public func kick(_ peerId: String) throws {
        try requireMember()
        try requireHost()
        try requireParticipant(peerId)
        send(Out.kick(peerId))
    }

    public func lock(_ locked: Bool) throws {
        try requireMember()
        try requireHost()
        send(Out.lock(locked))
    }

    /// Ends the room for everyone.
    ///
    /// Only the host ends the room. The party ends when the relay confirms.
    public func end() throws {
        try requireMember()
        try requireHost()
        send(Out.end())
    }

    private func send(_ text: String) {
        link?.send(text)
    }

    private func requireMember() throws {
        if connection != .connected { throw PartyError(.notInRoom, "not connected to a room") }
    }

    private func requireHost() throws {
        if room?.participant(peerId)?.host != true { throw PartyError(.forbidden, "only the host may do this") }
    }

    private func requireController() throws {
        try requireMember()
        if media == nil { throw PartyError(.invalidMessage, "the room has no media") }
        if room?.participant(peerId)?.host != true && room?.control != .everyone {
            throw PartyError(.forbidden, "only the host controls playback")
        }
    }

    private func requireParticipant(_ peerId: String) throws {
        if room?.participant(peerId) == nil { throw PartyError(.invalidMessage, "no participant \(peerId)") }
    }

    /// No step until the relay answers the command, or 2 s (SDK.md §12).
    private func hold(_ type: String) {
        holdRe = type
        holdTimer?.cancel()
        holdTimer = clock.after(Owp.commandHoldMs) { [self] in endHold() }
    }

    private func endHold() {
        guard holdRe != nil else { return }
        holdRe = nil
        holdTimer?.cancel()
        holdTimer = nil
        step()
    }

    // MARK: Reconnect and resume (SDK.md §14)

    private func linkClosed(_ closedLink: Link, _ closeCode: Int?) {
        guard closedLink === link, connection == .connected else { return }
        if closeCode == Owp.closeReplaced {
            endParty(.replaced)
            return
        }
        connection = .reconnecting
        statusTimer?.cancel()
        statusTimer = nil
        heartbeat?.cancel()
        heartbeat = nil
        endHold()
        updatePlayer()
        publish()
        let myGeneration = generation
        let held = place!
        let resume = Out.resume(code: held.code, peerId: held.peerId, token: held.token)
        resumeWindow = clock.after(Owp.resumeMs) { [self] in
            if myGeneration == generation && connection == .reconnecting {
                endParty(.lost, PartyError(.unreachable, "could not resume within 5 minutes"))
            }
        }
        let open = openSocket
        reconnecting = spawn { [self] in
            var attempt = 0
            while myGeneration == generation {
                attempt += 1
                // floor(min(8000, 500 × 2^(n−1)) × (0.5 + 0.5u)) ms (SDK.md §14).
                let backoff = min(Owp.backoffMaxMs, Owp.backoffStartMs << min(attempt - 1, 10))
                let delayMs = Int64((Double(backoff) * (0.5 + 0.5 * draw())).rounded(.down))
                do {
                    try await clock.sleep(delayMs)
                } catch {
                    return
                }
                let outcome = await clock.race(timeoutMs: Owp.connectTimeoutMs, late: { $0.close(code: 1000) }) {
                    try await open(held.url)
                }
                guard case .value(let socket) = outcome else {
                    if Task.isCancelled { return }
                    continue
                }
                if Task.isCancelled {
                    socket.close(code: 1000)
                    return
                }
                let next = Link(url: held.url, socket: socket, clock: clock)
                next.start()
                // Once `resume` is sent, the attempt outlives the end of the party: a `joined` gets `leave` (SDK.md
                // §14).
                resumeSent = true
                do {
                    // The clock sample matters only for going on: once the party ended, a `joined` leaves at once.
                    try await admit(next, resume, "resume", needClock: { [self] in myGeneration == generation }) {
                        [self] joined in
                        if myGeneration == generation {
                            joinedOn(next, joined, resumed: true)
                        } else {
                            next.send(Out.leave())
                            next.close(1000)
                        }
                    }
                    if myGeneration == generation {
                        resumeSent = false
                        reconnecting = nil
                    }
                    return
                } catch is CancellationError {
                    next.close(1000)
                    if myGeneration == generation { resumeSent = false }
                    return
                } catch {
                    // Any other failure is a failed attempt.
                    next.close(1000)
                    if myGeneration == generation { resumeSent = false }
                    let code = (error as? PartyError)?.code
                    if code == .resumeFailed || code == .clientBlocked {
                        if myGeneration == generation { endParty(.lost, error as? PartyError) }
                        return
                    }
                }
            }
        }
    }

    // MARK: Ending

    /// Leaves the room for good, and the party ends with `EndReason.left`.
    ///
    /// During `create` or `join`, it cancels them instead. They fail with `cancelled`, and the party returns to idle.
    ///
    /// Outside a room, the call does nothing.
    public func leave() {
        switch connection {
        case .connected:
            link?.send(Out.leave())
            link?.close(1000)
            endParty(.left)
        case .reconnecting:
            link?.close(1000)
            endParty(.left)
        case .connecting:
            cancelAttempt()
        case .idle, .ended:
            break
        }
    }

    /// The room's position now in the reference file's timeline, or nil without media or a clock sample.
    public func roomPositionMs() -> Int64? {
        guard let room, let media, let now = link?.relayNow() else { return nil }
        return room.position(durationMs: media.durationMs, at: now)
    }

    /// Leaves the room, if the party is in one, and releases everything.
    ///
    /// The party releases the adapter as `detachPlayer` does. Afterwards the party can't be used again, and `states()`
    /// and `events()` finish.
    ///
    /// A `create` or `join` that already reached the relay finishes in the background first, so that it can leave the
    /// room it may have joined.
    ///
    /// - Returns: A task that completes once no released adapter has a command queued or running, including a rate
    ///   restore. Await it before disposing of the player.
    @discardableResult
    public func close() -> Task<Void, Never> {
        if closed { return released() }
        leave()
        release()
        closed = true
        // Released adapters' lanes run out first, a rate restore included (SDK.md §11.2).
        let done = released()
        streamsFinished = true
        for continuation in stateSubscribers.values { continuation.finish() }
        for continuation in eventSubscribers.values { continuation.finish() }
        stateSubscribers.removeAll()
        eventSubscribers.removeAll()
        return done
    }

    /// SDK.md §7.4: the end releases the adapter, while the room's rate is still known for a rate restore.
    private func endParty(_ reason: EndReason, _ error: PartyError? = nil) {
        release()
        link?.close(1000)
        resetSession()
        connection = .ended
        endReason = reason
        publish()
        emit(EndedEvent(reason: reason, error: error))
    }

    /// Forgets the room and stops every timer; the next session starts over. The adapter stays attached, with its
    /// ref: only `detachPlayer`, `attachPlayer` with another adapter, the end and `close()` release it (SDK.md §7.2,
    /// §11.2).
    private func resetSession() {
        unbind()
        generation += 1
        startSent = false
        tick?.cancel()
        tick = nil
        // A resume attempt that sent `resume` waits for its answer, and a `joined` waiting for its clock leaves now.
        resumeCheck?.run()
        if !resumeSent { reconnecting?.cancel() }
        resumeCheck = nil
        reconnecting = nil
        resumeSent = false
        resumeWindow?.cancel()
        resumeWindow = nil
        cancelMatching()
        playTimer?.cancel()
        playTimer = nil
        holdTimer?.cancel()
        holdTimer = nil
        holdRe = nil
        statusTimer?.cancel()
        statusTimer = nil
        heartbeat?.cancel()
        heartbeat = nil
        link = nil
        place = nil
        room = nil
        epoch = 0
        media = nil
        own = nil
        ownEpoch = false
        pendingSetMedia = nil
        match = nil
        choice = nil
        mismatch = nil
        candidates = []
        offsetMs = 0
        away = false
        lastSeekAtMs = nil
        stepWanted = false
        playerState = .idle
        lastStatus = nil
        lastStatusAtMs = nil
        statusForced = false
    }

    private func publish() {
        let next = PartyState(
            connection: connection,
            peerId: place?.peerId,
            room: room,
            media: media,
            match: media != nil ? match : nil,
            choice: choice,
            mismatch: mismatch,
            candidates: candidates,
            offsetMs: offsetMs,
            player: playerState,
            endReason: endReason
        )
        if next == state { return }
        state = next
        for continuation in stateSubscribers.values { continuation.yield(next) }
    }

    private func emit(_ event: any PartyEvent) {
        for continuation in eventSubscribers.values {
            // Under a bound `Work`, an event counts until the subscriber took it.
            if case .enqueued = continuation.yield(event) { Work.current?.add(1) }
        }
    }

    // MARK: Types

    /// `media` as this party last sent it in `setMedia`, and `choice`, the app's file for it.
    private struct OwnMedia {
        let media: Media
        let choice: Candidate<Ref>
    }

    /// One adapter command. A `seek` is in flight from its queueing to its end; a `restore` stays queued through
    /// another release of its adapter.
    private final class Command {
        let seek: Bool
        let restore: Bool
        let run: @MainActor (any PlayerAdapter) async throws -> Void

        init(seek: Bool, restore: Bool, run: @escaping @MainActor (any PlayerAdapter) async throws -> Void) {
            self.seek = seek
            self.restore = restore
            self.run = run
        }
    }

    /// One adapter's commands, run one at a time, in order, each awaited; lanes of different adapters never wait.
    private final class Lane {
        let player: any PlayerAdapter
        var queue: [Command] = []
        var running: Command?

        init(player: any PlayerAdapter) { self.player = player }
    }
}

/// How the wait for `joined` ended.
private enum Admission: Sendable {
    case joined
    case failed(PartyError)
    case timedOut
    case cancelled
}

/// The running resume attempt's re-check of its answer, compared by identity.
@MainActor
private final class ResumeCheck {
    let run: () -> Void

    init(_ run: @escaping () -> Void) { self.run = run }
}

/// The probe of a `choose` while it runs.
@MainActor
private final class ManualProbe {
    let cancel: () -> Void

    init(_ cancel: @escaping () -> Void) { self.cancel = cancel }
}

/// The party's place in its room: the relay URL, and what a `resume` sends.
private struct Place {
    let url: String
    let code: String
    let peerId: String
    let token: String
}

/// The connections of a `create` race (SDK.md §6), filled in as they open, and its outcome: the index of the first
/// connection with a clock sample, or nil once every connection failed.
@MainActor
private final class Race {
    var links: [Link?]
    let winner = Waiter<Int?>()
    private var failures = 0

    init(count: Int) { links = Array(repeating: nil, count: count) }

    func failed() {
        failures += 1
        if failures == links.count { winner.resume(nil) }
    }
}
