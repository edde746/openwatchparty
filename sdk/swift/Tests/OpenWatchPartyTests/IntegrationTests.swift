// Whole parties against a real relay. Only macOS and Linux can start the relay, so the suite is left out elsewhere.
#if os(macOS) || os(Linux)
import Foundation
import Testing
@testable import OpenWatchParty

private let heat = Media(kind: .movie, title: "Heat", durationMs: 600_000, ids: ["imdb": "tt0113277"])

/// Finds `heat` 300 ms longer than the reference.
private func findsHeat() -> Resolver<String> {
    Resolver { media in
        sameTitle(media, heat) ? [Candidate(id: "heat", ref: "heat.mkv", durationMs: media.durationMs + 300)] : []
    }
}

private func near(_ x: Int64, _ y: Int64, _ tolerance: Int64) -> Bool { abs(x - y) <= tolerance }

/// Whole parties with the SDK's default `URLSession` transport against a relay built from relay/, a fresh one per
/// test, in test mode (relay timers × 0.1).
@MainActor
@Suite(.serialized, .relayBinary, .timeLimit(.minutes(1)))
struct IntegrationTests {
    private struct Watching {
        let a: WatchParty<String>
        let b: WatchParty<String>
        let pa: RealTimePlayer
        let pb: RealTimePlayer
    }

    /// Anna creates, Ben joins, Anna shares `heat` at 60 s; returns once both players run.
    private func watching(_ h: RelayHarness, _ relays: Relays) async throws -> Watching {
        let a = try h.party(relays, "Anna")
        let b = try h.party(relays, "Ben", findsHeat())
        let code = try await a.create()
        try await b.join(code)
        let pa = RealTimePlayer(clock: h.clock, durationMs: heat.durationMs)
        let pb = RealTimePlayer(clock: h.clock, durationMs: heat.durationMs + 300)
        try a.setMedia(heat, ref: "heat.mkv", positionMs: 60_000)
        a.attachPlayer(pa, ref: "heat.mkv")
        try await h.eventually("Ben's match") { b.state.choice != nil }
        #expect(b.state.match == Match(.matched, deltaMs: 300))
        b.attachPlayer(pb, ref: "heat.mkv")
        try await h.eventually("both players running") { a.state.room?.phase == .playing && !pa.paused && !pb.paused }
        return Watching(a: a, b: b, pa: pa, pb: pb)
    }

    @Test func createAndJoinWithATypedCode() async throws {
        try await RelayHarness.run { h in
            let relay = try await h.relay()
            let relays = try h.relaysAt(relay.port)
            let a = try h.party(relays, "Anna")
            let b = try h.party(relays, "Ben")
            let aEvents = h.record(a)
            let code = try await a.create()
            #expect(code.count == 6 && code.allSatisfy { Owp.alphabet.contains($0) }, "\(code)")
            #expect(a.state.connection == .connected)
            #expect(a.state.isHost)

            // Typed as a user would: lower case, with a hyphen.
            try await b.join(try formatCode(code).lowercased())
            #expect(b.state.room?.code == code)
            #expect(!b.state.isHost)
            try await h.eventually("Anna sees Ben") { a.state.room?.participants.count == 2 }
            let joined = aEvents.of(ParticipantJoinedEvent.self)
            #expect(joined.map(\.participant.name) == ["Ben"])
            #expect(joined.first?.participant.client == "OwpSwiftTest/1.0")

            // The host ends the room, then brings the group back to its code, typed the same way.
            try a.end()
            try await h.eventually("the room ended") { a.state.connection == .ended }
            try await a.join(try formatCode(code).lowercased(), create: true)
            #expect(a.state.room?.code == code)
            #expect(a.state.isHost)
            #expect(a.state.room?.participants.map(\.name) == ["Anna"])
        }
    }

    @Test func setMediaMatchesAndBothPlayersFollow() async throws {
        try await RelayHarness.run { h in
            let relay = try await h.relay()
            let w = try await watching(h, try h.relaysAt(relay.port))
            try await h.eventually("both ready") {
                w.a.state.room?.participants.map(\.player) == [.ready, .ready] && w.b.state.player == .ready
            }
            try await Task.sleep(for: .seconds(1))
            let room = try #require(w.a.roomPositionMs())
            #expect(near(w.pa.positionMs, room, 300), "Anna at \(w.pa.positionMs), room at \(room)")
            #expect(near(w.pb.positionMs, room, 300), "Ben at \(w.pb.positionMs), room at \(room)")
            #expect(room > 60_500, "the room plays from 60 s: \(room)")
        }
    }

    @Test func pauseSeekAndRateReachTheOtherPlayer() async throws {
        try await RelayHarness.run { h in
            let relay = try await h.relay()
            let w = try await watching(h, try h.relaysAt(relay.port))
            let bEvents = h.record(w.b)

            try w.a.pause()
            try await h.eventually("both paused") { w.pa.paused && w.pb.paused && w.b.state.room?.phase == .paused }
            #expect(near(w.pa.positionMs, w.pb.positionMs, 600), "Anna \(w.pa.positionMs), Ben \(w.pb.positionMs)")
            let pause = try #require(bEvents.of(ActionEvent.self).first { $0.kind == "pause" })
            #expect(pause.byPeerId == w.a.state.peerId)
            #expect(pause.by?.name == "Anna")
            #expect(!pause.byMe)

            try w.a.seek(to: 120_000)
            try await h.eventually("Ben at 120 s") { w.b.state.room?.positionMs == 120_000 && w.pb.positionMs == 120_000 }
            #expect(w.pa.positionMs == 120_000)

            try w.a.setRate(1.5)
            try await h.eventually("Ben at rate 1.5") { w.b.state.room?.rate == 1.5 && w.pb.rate == 1.5 }
            #expect(bEvents.of(ActionEvent.self).map(\.kind) == ["pause", "seek", "rate"])
        }
    }

    @Test func resumeAfterADroppedConnection() async throws {
        try await RelayHarness.run { h in
            let relay = try await h.relay()
            let proxy = try h.proxy(to: relay.port)
            let w = try await watching(h, try h.relaysAt(proxy.port))
            let peers = [w.a.state.peerId, w.b.state.peerId]
            let code = w.a.state.room?.code
            let aEvents = h.record(w.a)

            proxy.cut()
            try await h.eventually("reconnecting") { w.a.state.connection == .reconnecting }
            try await h.eventually("resumed") {
                w.a.state.connection == .connected && w.b.state.connection == .connected
                    && w.a.state.room?.participants.allSatisfy(\.connected) == true
            }
            #expect([w.a.state.peerId, w.b.state.peerId] == peers)
            #expect(w.a.state.room?.code == code)
            #expect(!aEvents.events.contains { $0 is EndedEvent || $0 is ParticipantLeftEvent })

            // The room paused while nobody was connected; it plays again for both. The new connections' clocks have
            // few samples yet, so a busy machine starts them apart; the rate nudge brings them together.
            try w.a.play()
            try await h.eventually("both playing") { w.b.state.room?.phase == .playing && !w.pa.paused && !w.pb.paused }
            try await h.eventually("in step") { near(w.pa.positionMs, w.pb.positionMs, 400) }
        }
    }
}
#endif
