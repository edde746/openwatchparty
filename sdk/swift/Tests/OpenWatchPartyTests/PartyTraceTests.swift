import Foundation
import Testing
@testable import OpenWatchParty

/// Runs every trace of conformance/vectors/party.json (SDK.md §17): a real `WatchParty` against a scripted relay,
/// scripted adapters and a scripted resolver, on a `VirtualClock`. conformance/vectors/README.md (the party section)
/// defines the format; this file is its Swift driver. Settling is exact: the trace runs under a bound `Work`, so the
/// clock runs until every task the SDK started is parked or done.
@MainActor
@Suite struct PartyTraceTests {
    @Test(arguments: try PartyTrace.all())
    func trace(_ trace: PartyTrace) async throws {
        let work = Work()
        try await Work.$current.withValue(work) {
            let run = try TraceRun(trace.setup, work)
            await run.run(trace)
        }
    }
}

/// One case of party.json.
struct PartyTrace: Sendable, CustomTestStringConvertible {
    let name: String
    let setup: JSON
    let steps: [JSON]
    let outputs: [JSON]

    var testDescription: String { name }

    static func all() throws -> [PartyTrace] {
        guard let doc = JSON.parse(try TestData.vectors("party")), doc["table"]?.string == "party",
              let cases = doc["cases"]?.array, !cases.isEmpty else { throw TestFailure("party.json holds no party table") }
        return cases.map { c in
            PartyTrace(
                name: c["name"]?.string ?? "?",
                setup: c["input"]?["setup"] ?? .null,
                steps: c["input"]?["steps"]?.array ?? [],
                outputs: c["expect"]?["outputs"]?.array ?? []
            )
        }
    }
}

// MARK: The trace's world

/// The channels of an output, in the README's order; `state` is read, not recorded.
private let channels = ["send", "command", "event", "resolver", "net", "done"]

/// One trace's world: the party, its fakes and what the party did since the last step.
@MainActor
private final class TraceRun {
    let setup: JSON
    let work: Work
    let clock: VirtualClock
    private(set) var relays: Relays!
    private(set) var party: WatchParty<String>!
    /// Per channel, what the party did in this step.
    var out: [String: [JSONValue]] = [:]
    private var sockets: [TraceSocket] = []
    private var players: [String: TracePlayer] = [:]
    /// How the next connection opens, per URL, and for any URL under "".
    private var nextOpen: [String: String] = [:]
    /// The round trip of pings sent from now on, per URL, else `rttAll`; nil never answers.
    private var rtt: [String: Int64?] = [:]
    private var rttAll: Int64?
    private var nextGet: JSON?
    private let draws: DrawSource
    private var resolving: (title: String, waiter: Waiter<Result<[Candidate<String>], any Error>>)?
    private var probes: [String: Waiter<Result<Int64?, any Error>>] = [:]
    private var calls: [String] = []

    init(_ setup: JSON, _ work: Work) throws {
        self.setup = setup
        self.work = work
        clock = VirtualClock(startMs: setup["startMs"]?.int ?? 0)
        rttAll = setup["rttMs"]?.int
        draws = DrawSource(setup["random"]?.array?.compactMap(\.number) ?? [])
        if let base = setup["base"]?.string {
            relays = try Relays.at(base)
        } else {
            relays = Relays.public(httpGet: { [unowned self] url in try await get(url) }, clock: clock)
        }
        let resolver = Resolver<String> { [unowned self] media in try await resolve(media) }
        party = try WatchParty(
            relays: relays,
            client: setup["client"]?.string ?? "",
            name: setup["name"]?.string ?? "",
            resolver: setup["resolver"]?.bool == true ? resolver : nil,
            connect: { [unowned self] url in try await connect(url) },
            clock: clock,
            random: ScriptedRandom(source: draws)
        )
        // Not counted itself: each event it takes was counted when the party handed it over.
        let events = party.events()
        Task { [weak self, work] in
            for await event in events {
                self?.record("event", project(event))
                work.add(-1)
            }
        }
    }

    func record(_ channel: String, _ entry: JSONValue) { out[channel, default: []].append(entry) }

    /// Runs every step, compares its outputs, then checks the end of the trace and closes the party.
    func run(_ trace: PartyTrace) async {
        await clock.run()
        var failed = false
        for (i, step) in trace.steps.enumerated() {
            await self.step(step)
            let want = expected(i < trace.outputs.count ? trace.outputs[i] : .null)
            let got = take(want)
            if got != want {
                Issue.record("step \(i) \(JSONValue(step)) at local \(clock.now):\n got \(got)\nwant \(want)")
                failed = true
                break
            }
            if draws.missing {
                Issue.record("step \(i): the party drew more random values than setup.random holds")
                failed = true
                break
            }
            if let u = draws.unreproducible {
                Issue.record("step \(i): Double.random(in: 0..<1) can't yield the draw \(u): not a multiple of 2^-53")
                failed = true
                break
            }
        }
        if !failed {
            #expect(draws.left.isEmpty, "setup.random holds draws the party never took: \(draws.left)")
            #expect(resolving == nil, "a resolve call is still unanswered")
            #expect(probes.isEmpty, "a probe is still unanswered: \(probes.keys.sorted())")
            for (id, p) in players.sorted(by: { $0.key < $1.key }) {
                #expect(p.held == nil, "\(id) still runs a held command")
            }
            let answered = Set(trace.outputs.flatMap { $0["done"]?.array ?? [] }.compactMap { $0["id"]?.string })
            for id in calls { #expect(answered.contains(id), "call \(id) never settled") }
        }
        party.close()
        await clock.run()
    }

    // MARK: Steps

    func step(_ step: JSON) async {
        guard let s = step.object else {
            Issue.record("a step is no object: \(JSONValue(step))")
            return
        }
        // A Swift `String` can't carry an unpaired surrogate, so neither relay text nor an app's value can hold one.
        if step.holdsUnpaired {
            Issue.record("the step holds an unpaired surrogate, which the Swift driver can't pass on: \(JSONValue(step))")
            return
        }
        let on = s["on"]?.string
        if s["call"] != nil {
            call(s)
        } else if let msg = s["recv"] {
            socket(on)?.deliver(JSONValue(msg).description)
        } else if let text = s["recvText"]?.string {
            socket(on)?.deliver(text)
        } else if let code = s["drop"] {
            socket(on)?.drop(code.int.map { Int($0) })
        } else if let how = s["nextOpen"]?.string {
            nextOpen[on ?? ""] = how
        } else if let ms = s["rtt"] {
            if let on {
                rtt.updateValue(ms.int, forKey: on)
            } else {
                rttAll = ms.int
                rtt = [:]
            }
        } else if let doc = s["nextGet"] {
            nextGet = doc
        } else if let ms = s["advance"]?.int {
            await clock.advance(by: ms)
            return
        } else if let id = s["player"]?.string {
            let p = players[id] ?? TracePlayer(id, self)
            players[id] = p
            p.set(s)
        } else if let id = s["hold"]?.string {
            player(id)?.holdNext = true
        } else if let id = s["finish"]?.string {
            guard let p = player(id), let held = p.held else {
                Issue.record("\(id) has no running command to finish")
                return
            }
            p.held = nil
            if s["throws"]?.bool == true {
                held.waiter.resume(true)
            } else {
                held.apply()
                held.waiter.resume(false)
            }
        } else if s["answer"] != nil || s["answerThrows"] != nil {
            guard let pending = resolving else {
                Issue.record("no resolve call to answer")
                return
            }
            resolving = nil
            if let answer = s["answer"]?.array {
                pending.waiter.resume(.success(answer.map(candidate)))
            } else {
                pending.waiter.resume(.failure(TestFailure("the resolver failed")))
            }
        } else if let id = s["probeAnswer"]?.string ?? s["probeThrows"]?.string {
            guard let pending = probes.removeValue(forKey: id) else {
                Issue.record("no running probe of \(id)")
                return
            }
            if s["probeAnswer"] != nil {
                pending.resume(.success(s["durationMs"]?.int))
            } else {
                pending.resume(.failure(TestFailure("the probe of \(id) failed")))
            }
        } else {
            Issue.record("unknown step \(JSONValue(step))")
        }
        await clock.run()
    }

    private func call(_ s: [String: JSON]) {
        let id = s["id"]?.string
        if let id { calls.append(id) }
        func ok(_ value: JSONValue?) {
            guard let id else { return }
            record("done", value.map { ["id": .string(id), "value": $0] } ?? ["id": .string(id)])
        }
        func failed(_ error: any Error) {
            guard let error = error as? PartyError else {
                Issue.record("\(s["call"]?.string ?? "?") threw \(error)")
                return
            }
            guard let id else {
                Issue.record("\(s["call"]?.string ?? "?") without an id threw \(error)")
                return
            }
            record("done", [
                "id": .string(id), "error": .string(error.code.rawValue), "re": error.re.map { .string($0) } ?? .null,
                "message": .string(error.message),
            ])
        }
        /// Runs `op` as a task of its own: `done` once it settles.
        func later(_ op: @escaping @MainActor () async throws -> JSONValue?) {
            spawn {
                do {
                    ok(try await op())
                } catch {
                    failed(error)
                }
            }
        }
        /// Argument `name` as Swift's API takes it; nil, with an issue, when it is no number an `Int64` holds.
        func int64(_ name: String) -> Int64? {
            guard let d = s[name]?.number, let v = Int64(exactly: d) else {
                Issue.record("\(s["call"]?.string ?? "?") needs \(name) as an integer an Int64 holds")
                return nil
            }
            return v
        }
        let party = party!
        do {
            switch s["call"]?.string {
            case "create":
                return later { .string(try await party.create()) }
            case "join":
                let code = s["code"]?.string ?? ""
                let create = s["create"]?.bool ?? false
                return later {
                    try await party.join(code, create: create)
                    return nil
                }
            case "choose":
                let candidate = candidate(s["candidate"] ?? .null)
                return later { .bool(await party.choose(candidate)) }
            case "refresh":
                let relays = relays!
                return later { .bool(await relays.refresh()) }
            case "close":
                let done = party.close()
                return later { [work] in
                    await work.join(done)
                    return nil
                }
            case "detachPlayer":
                let done = party.detachPlayer(s["player"]?.string.flatMap(player))
                return later { [work] in
                    await work.join(done)
                    return nil
                }
            case "attachPlayer":
                guard let p = s["player"]?.string.flatMap(player) else { return }
                party.attachPlayer(p, ref: s["ref"]?.string ?? "")
            case "setMedia":
                guard let media = s["media"]?.object.flatMap(readMedia)?.media else {
                    Issue.record("no Media holds \(JSONValue(s["media"] ?? .null))")
                    return
                }
                guard let positionMs = s["positionMs"] == nil ? 0 : int64("positionMs") else { return }
                try party.setMedia(media, ref: s["ref"]?.string ?? "", positionMs: positionMs)
            case "clearMedia": try party.clearMedia()
            case "play": try party.play()
            case "pause": try party.pause()
            case "seek":
                guard let ms = int64("ms") else { return }
                try party.seek(to: ms)
            case "setRate": try party.setRate(s["rate"]?.number ?? .nan)
            case "setOffset":
                guard let ms = int64("ms") else { return }
                try party.setOffset(ms)
            case "setControl":
                guard let mode = s["mode"]?.string.flatMap(Control.init(rawValue:)) else {
                    Issue.record("no control mode in \(JSONValue(.object(s, unpaired: false)))")
                    return
                }
                try party.setControl(mode)
            case "lock": try party.lock(s["locked"]?.bool ?? false)
            case "kick": try party.kick(s["peerId"]?.string ?? "")
            case "transferHost": try party.transferHost(s["peerId"]?.string ?? "")
            case "end": try party.end()
            case "leave": party.leave()
            case "acceptMismatch": party.acceptMismatch()
            case "declineMismatch": party.declineMismatch()
            case "resolveAgain": party.resolveAgain()
            case "playerChanged": party.playerChanged()
            case let other:
                Issue.record("unknown call \(other ?? "nil")")
                return
            }
            ok(nil)
        } catch {
            failed(error)
        }
    }

    /// A candidate of a trace: `ref` is `id` unless given; `"probe": true` gives it a scripted probe.
    private func candidate(_ c: JSON) -> Candidate<String> {
        let id = c["id"]?.string ?? ""
        var probe: (@MainActor @Sendable () async throws -> Int64?)?
        if c["probe"]?.bool == true { probe = { [unowned self] in try await self.probe(id) } }
        return Candidate(
            id: id, ref: c["ref"]?.string ?? id, durationMs: c["durationMs"]?.int, sizeBytes: c["sizeBytes"]?.int,
            oshash: c["oshash"]?.string, name: c["name"]?.string, probe: probe
        )
    }

    private func player(_ id: String) -> TracePlayer? {
        guard let p = players[id] else {
            Issue.record("unknown player \(id)")
            return nil
        }
        return p
    }

    /// The connection opened last that is still open, to `on` when given.
    private func socket(_ on: String?) -> TraceSocket? {
        guard let s = sockets.last(where: { $0.isOpen && (on == nil || $0.url == on) }) else {
            Issue.record("no open connection\(on.map { " to \($0)" } ?? "")")
            return nil
        }
        return s
    }

    // MARK: The party's seams

    private func connect(_ url: String) async throws -> any WebSocketConnection {
        record("net", ["open": .string(url)])
        let key = nextOpen[url] != nil ? url : ""
        switch nextOpen.removeValue(forKey: key) {
        case "fail":
            throw TestFailure("\(url) refused the connection")
        case "hang":
            _ = await park(Waiter<Void>(), cancelled: ())
            throw CancellationError()
        default:
            let socket = TraceSocket(url, self)
            sockets.append(socket)
            return socket
        }
    }

    /// A GET of the list's source: `setup.list` unless a `nextGet` step scripted this one.
    private func get(_ url: String) async throws -> String {
        record("net", ["get": .string(url)])
        let how = nextGet ?? setup["list"] ?? .null
        nextGet = nil
        switch how.string {
        case "fail":
            throw TestFailure("GET \(url) failed")
        case "hang":
            _ = await park(Waiter<Void>(), cancelled: ())
            throw CancellationError()
        default:
            return JSONValue(how).description
        }
    }

    /// Answers a ping its URL's round trip later, with relay time = send time + rtt / 2 + `relayOffsetMs`.
    func sent(_ text: String, on socket: TraceSocket) {
        guard let msg = try? JSONValue(parsing: text) else {
            Issue.record("the party sent no JSON: \(text)")
            return
        }
        guard msg["type"]?.string == "ping" else {
            record("send", ["on": .string(socket.url), "msg": msg])
            return
        }
        guard let rtt = rtt[socket.url] ?? rttAll else { return }
        let serverTime = clock.now + rtt / 2 + (setup["relayOffsetMs"]?.int ?? 0)
        let pong: JSONValue = ["type": "pong", "id": msg["id"] ?? .null, "serverTime": .number(Double(serverTime))]
        _ = clock.schedule(afterMs: rtt) { [weak socket] in socket?.deliver(pong.description) }
    }

    private func resolve(_ media: Media) async throws -> [Candidate<String>] {
        if resolving != nil { Issue.record("a second resolve call while one is pending") }
        let title = media.title
        record("resolver", ["resolve": .string(title)])
        let waiter = Waiter<Result<[Candidate<String>], any Error>>()
        resolving = (title, waiter)
        return try await park(waiter, cancelled: .failure(CancellationError())) { [self] in
            if resolving?.waiter === waiter { resolving = nil }
            record("resolver", ["resolveCancelled": .string(title)])
        }.get()
    }

    private func probe(_ id: String) async throws -> Int64? {
        if probes[id] != nil { Issue.record("probe \(id) called twice at once") }
        record("resolver", ["probe": .string(id)])
        let waiter = Waiter<Result<Int64?, any Error>>()
        probes[id] = waiter
        return try await park(waiter, cancelled: .failure(CancellationError())) { [self] in
            if probes[id] === waiter { probes[id] = nil }
            record("resolver", ["probeCancelled": .string(id)])
        }.get()
    }

    // MARK: Outputs

    /// This step's outputs as `want` writes them: a sent message with its connection only where `want` pins it, an
    /// error's `re` and `message` only where `want` has them, `net` in URL order, `done` in id order, and the state
    /// fields `want` names.
    private func take(_ want: JSONValue) -> JSONValue {
        var got: [String: JSONValue] = [:]
        for channel in channels {
            guard let entries = out[channel], !entries.isEmpty else { continue }
            let wanted = want[channel]?.arrayValue ?? []
            got[channel] = .array(entries.enumerated().map { i, entry in
                // A done entry matches the expected one of its call; the others by position.
                let w: JSONValue?
                if channel == "done" {
                    w = wanted.first { $0["id"] == entry["id"] }
                } else {
                    w = i < wanted.count ? wanted[i] : nil
                }
                switch channel {
                case "send":
                    return w?["on"] != nil && w?["type"] == nil ? entry : entry["msg"] ?? .null
                case "done" where entry["error"] != nil:
                    var e = entry.objectValue ?? [:]
                    if w?["re"] == nil { e["re"] = nil }
                    if w?["message"] == nil { e["message"] = nil }
                    return .object(e)
                default:
                    return entry
                }
            })
        }
        if let net = got["net"] { got["net"] = byUrl(net) }
        if let done = got["done"] { got["done"] = byId(done) }
        if let fields = want["state"]?.objectValue?.keys {
            got["state"] = .object(Dictionary(uniqueKeysWithValues: fields.map { ($0, state($0)) }))
        }
        out = [:]
        return .object(got)
    }

    /// A state member as the README projects it.
    private func state(_ field: String) -> JSONValue {
        let state = party.state
        func int(_ x: Int64?) -> JSONValue { x.map { .number(Double($0)) } ?? .null }
        switch field {
        case "connection": return .string("\(state.connection)")
        case "peerId": return state.peerId.map { .string($0) } ?? .null
        case "room": return int(state.room?.rev)
        case "media": return state.media.map { .string($0.title) } ?? .null
        case "match":
            guard let m = state.match else { return .null }
            return m.deltaMs.map { ["state": .string(m.state.rawValue), "deltaMs": int($0)] } ?? ["state": .string(m.state.rawValue)]
        case "choice": return state.choice.map { ["id": .string($0.id), "ref": .string($0.ref)] } ?? .null
        case "mismatch":
            return state.mismatch.map { ["id": .string($0.candidate.id), "deltaMs": int($0.deltaMs)] } ?? .null
        case "candidates": return .array(state.candidates.map { ["id": .string($0.candidate.id), "deltaMs": int($0.deltaMs)] })
        case "offsetMs": return int(state.offsetMs)
        case "player": return .string(state.player.rawValue)
        case "endReason": return state.endReason.map { .string("\($0)") } ?? .null
        case "isHost": return .bool(state.isHost)
        case "canControl": return .bool(state.canControl)
        case "roomPositionMs": return int(party.roomPositionMs())
        default:
            Issue.record("unknown state field \(field)")
            return .null
        }
    }

    // MARK: Waiting for the trace

    /// Waits for `waiter`. When the waiting task is cancelled first, `cancelled` settles it and `noted` runs, in a task
    /// of its own that starts at the cancellation, so it comes before anything the canceller starts after.
    private func park<T: Sendable>(
        _ waiter: Waiter<T>,
        cancelled: T,
        noted: @escaping @MainActor () -> Void = {}
    ) async -> T {
        await withTaskCancellationHandler {
            await waiter.wait { {} }
        } onCancel: {
            spawn { if waiter.resume(cancelled) { noted() } }
        }
    }
}

/// The expected outputs of a step with every rate rounded as the driver records it, `net` in URL order and `done` in
/// id order.
private func expected(_ outputs: JSON) -> JSONValue {
    var want = JSONValue(outputs).objectValue ?? [:]
    if let commands = want["command"]?.arrayValue {
        want["command"] = .array(commands.map { c in
            guard c["do"]?.string == "setRate", let rate = c["rate"]?.number else { return c }
            return c.with(["rate": .number(round6(rate))])
        })
    }
    if let net = want["net"] { want["net"] = byUrl(net) }
    if let done = want["done"] { want["done"] = byId(done) }
    return .object(want)
}

/// `net` entries are pinned per URL only: a stable sort by URL keeps each URL's order and drops the rest.
private func byUrl(_ net: JSONValue) -> JSONValue {
    sorted(net) { ($0["get"] ?? $0["open"] ?? $0["close"])?.string ?? "" }
}

/// `done` entries aren't ordered at all, and each call reports once, so sorting by id drops their order.
private func byId(_ done: JSONValue) -> JSONValue {
    sorted(done) { $0["id"]?.string ?? "" }
}

/// `entries` sorted stably by `key`, in UTF-8 order.
private func sorted(_ entries: JSONValue, by key: (JSONValue) -> String) -> JSONValue {
    .array((entries.arrayValue ?? []).enumerated().sorted { a, b in
        let (ka, kb) = (key(a.element), key(b.element))
        return ka == kb ? a.offset < b.offset : JSONWriter.utf8Less(ka, kb)
    }.map(\.element))
}

private func round6(_ rate: Double) -> Double { (rate * 1e6).rounded() / 1e6 }

private func project(_ event: any PartyEvent) -> JSONValue {
    func string(_ s: String?) -> JSONValue { s.map { .string($0) } ?? .null }
    switch event {
    case let e as ActionEvent:
        return [
            "type": "action", "kind": .string(e.kind), "byPeerId": string(e.byPeerId), "by": string(e.by?.peerId),
            "byMe": .bool(e.byMe), "room": .number(Double(e.room.rev)),
        ]
    case let e as ParticipantJoinedEvent:
        return ["type": "participantJoined", "peerId": .string(e.participant.peerId)]
    case let e as ParticipantLeftEvent:
        return ["type": "participantLeft", "peerId": .string(e.participant.peerId)]
    case let e as ErrorEvent:
        return ["type": "error", "code": .string(e.error.code.rawValue), "re": string(e.error.re)]
    case let e as ResolveFailedEvent:
        return ["type": "resolveFailed", "timeout": .bool(e.error is TimeoutError)]
    case is PlayBlockedEvent:
        return ["type": "playBlocked"]
    case let e as EndedEvent:
        return ["type": "ended", "reason": .string("\(e.reason)"), "error": string(e.error?.code.rawValue)]
    default:
        return ["type": .string("unknown \(type(of: event))")]
    }
}

extension JSONValue {
    var arrayValue: [JSONValue]? {
        if case .array(let a) = self { return a }
        return nil
    }

    var number: Double? {
        if case .number(let d) = self { return d }
        return nil
    }
}

// MARK: Fakes

/// The draws `setup.random` scripts, in order.
private final class DrawSource {
    var left: [Double]
    /// The party drew one more than the trace holds.
    var missing = false
    /// A draw `Double.random(in: 0..<1)` can't yield.
    var unreproducible: Double?

    init(_ draws: [Double]) { left = draws }
}

/// Yields the scripted u through `Double.random(in: 0..<1, using:)`, which reads one value's low 53 bits as u × 2^53.
private struct ScriptedRandom: RandomNumberGenerator {
    let source: DrawSource

    mutating func next() -> UInt64 {
        guard !source.left.isEmpty else {
            source.missing = true
            return 0
        }
        let u = source.left.removeFirst()
        guard let bits = UInt64(exactly: u * 0x1p53) else {
            source.unreproducible = u
            return 0
        }
        return bits
    }
}

/// A relay connection: pings are answered by the trace, everything else the party sends is an output.
@MainActor
private final class TraceSocket: WebSocketConnection {
    let url: String
    private unowned let run: TraceRun
    private var inbox: [String] = []
    private var reading: Waiter<String?>?
    private var code: Int?
    /// False once either side closed it.
    private(set) var isOpen = true

    init(_ url: String, _ run: TraceRun) {
        self.url = url
        self.run = run
    }

    func receive() async -> String? {
        if !inbox.isEmpty { return inbox.removeFirst() }
        if !isOpen { return nil }
        let waiter = Waiter<String?>()
        reading = waiter
        return await waiter.wait { {} }
    }

    func closeCode() async -> Int? { code }

    func send(_ text: String) {
        if isOpen { run.sent(text, on: self) }
    }

    /// The party closing the connection.
    func close(code: Int) {
        guard isOpen else { return }
        run.record("net", ["close": .string(url), "code": .number(Double(code))])
        end()
    }

    /// The relay sending `text`.
    func deliver(_ text: String) {
        guard isOpen else { return }
        if let reading {
            self.reading = nil
            reading.resume(text)
        } else {
            inbox.append(text)
        }
    }

    /// The relay closing the connection, with `code` or without a close status.
    func drop(_ code: Int?) {
        guard isOpen else { return }
        self.code = code
        end()
    }

    private func end() {
        isOpen = false
        reading?.resume(nil)
        reading = nil
    }
}

/// A scripted adapter. Its position moves at the rate last set while it plays; commands take effect when they finish.
@MainActor
private final class TracePlayer: PlayerAdapter {
    let id: String
    private unowned let run: TraceRun
    private var position: Int64 = 0
    private var since: Int64
    private var rate = 1.0
    private(set) var paused = true
    private(set) var buffering = false
    private(set) var durationMs: Int64?
    private(set) var firstFrameShown = true
    private(set) var canSetRate = false
    private(set) var seekAccuracyMs: Int64 = 0
    private var playThrows = false
    /// The next command stays running until a `finish` step.
    var holdNext = false
    /// The running held command: its effect, and its end (true: it throws).
    var held: (apply: () -> Void, waiter: Waiter<Bool>)?

    init(_ id: String, _ run: TraceRun) {
        self.id = id
        self.run = run
        since = run.clock.now
    }

    var positionMs: Int64 {
        if paused || buffering { return position }
        return position + Int64((Double(run.clock.now - since) * rate).rounded(.down))
    }

    /// Changes the readable state from now on with the members of a `player` step.
    func set(_ fields: [String: JSON]) {
        position = positionMs
        since = run.clock.now
        if let p = fields["positionMs"]?.int { position = p }
        if let b = fields["paused"]?.bool { paused = b }
        if let b = fields["buffering"]?.bool { buffering = b }
        if let d = fields["durationMs"] { durationMs = d.int }
        if let b = fields["firstFrameShown"]?.bool { firstFrameShown = b }
        if let b = fields["canSetRate"]?.bool { canSetRate = b }
        if let a = fields["seekAccuracyMs"]?.int { seekAccuracyMs = a }
        if let b = fields["playThrows"]?.bool { playThrows = b }
    }

    func play() async throws {
        try await command(["do": "play"], throws: playThrows) { self.set(["paused": .bool(false)]) }
    }

    func pause() async throws {
        try await command(["do": "pause"]) { self.set(["paused": .bool(true)]) }
    }

    func seek(to positionMs: Int64) async throws {
        try await command(["do": "seek", "positionMs": .number(Double(positionMs))]) {
            self.set(["positionMs": .number(Double(positionMs))])
        }
    }

    func setRate(_ rate: Double) async throws {
        try await command(["do": "setRate", "rate": .number(round6(rate))]) {
            self.set([:])
            self.rate = rate
        }
    }

    private func command(_ command: [String: JSONValue], throws: Bool = false, _ apply: @escaping () -> Void) async throws {
        var entry = command
        entry["player"] = .string(id)
        run.record("command", .object(entry))
        if holdNext {
            holdNext = false
            let waiter = Waiter<Bool>()
            held = (apply, waiter)
            if await waiter.wait({ {} }) { throw TestFailure("\(id) failed to \(command["do"]?.string ?? "?")") }
            return
        }
        if `throws` { throw TestFailure("\(id) refused to \(command["do"]?.string ?? "?")") }
        apply()
    }
}
