import Foundation
import Testing
@testable import OpenWatchParty

/// The session rules of SDK.md §7 and §8 that `party.json` doesn't pin, against a scripted relay, in virtual time.
@MainActor
@Suite(.timeLimit(.minutes(1)))
struct SessionTests {
    // MARK: join and leave (SDK.md §7)

    @Test func cancellingTheCallingTaskCancelsJoin() async throws {
        let f = try Fixture()
        let joining = Task { try await f.party.join("k7m-q2x") }
        let socket = try await f.relay.nextSocket()
        _ = try await socket.next("join")
        joining.cancel()
        await f.clock.run()
        await #expect(throws: CancellationError.self) { try await joining.value }
        #expect(f.state.connection == .idle)
        socket.reply(joined(rev: 1))
        _ = try await socket.next("leave")
        #expect(socket.closedByClient == 1000)
    }

    @Test func afterCloseCreateAndJoinThrowTheStateError() async throws {
        let f = try Fixture()
        f.party.close()
        await #expect(throws: PartyClosedError.self) { _ = try await f.party.create() }
        await #expect(throws: PartyClosedError.self) { try await f.party.join("K7M-Q2X") }
        await #expect(throws: PartyClosedError.self) { try await f.party.join("K7M-Q2") }
        #expect(f.relay.opened == 0)
    }

    @Test func closeFinishesTheStreamsAfterEnded() async throws {
        let s = try await joinedSession()
        let events = s.party.events()
        let states = s.party.states()
        s.party.close()
        var seen: [any PartyEvent] = []
        for await event in events { seen.append(event) }
        #expect(seen.count == 1)
        #expect((seen.first as? EndedEvent)?.reason == .left)
        var last: PartyState<String>?
        for await state in states { last = state }
        #expect(last?.connection == .ended)
    }

    // MARK: Relay input (SDK.md §8)

    @Test func aRelayMessageOf32768BytesAndMoreIsAccepted() async throws {
        let s = try await joinedSession()
        let padded = roomMessage(rev: 2, "p1", "p2", "p3").with(["padding": .string(String(repeating: "x", count: 32768))])
        #expect(padded.description.utf8.count > 32768)
        s.socket.reply(padded)
        await s.clock.run()
        #expect(s.state.room?.rev == 2)
        #expect(s.state.room?.participants.count == 3)
    }

    @Test func aSlowSubscriberGetsEveryEvent() async throws {
        let s = try await joinedSession()
        let slow = s.party.events()
        // 200 participants join, one snapshot each, while the subscriber reads nothing.
        var peers = ["p1", "p2"]
        for rev in Int64(2)...201 {
            peers.append("p\(rev + 1)")
            s.socket.reply(room(rev: rev, peers).with(["type": "room"]))
        }
        await s.clock.run()
        #expect(s.state.room?.participants.count == 202)
        var joinedPeers: [String] = []
        for await event in slow {
            joinedPeers.append((event as? ParticipantJoinedEvent)?.participant.peerId ?? "?")
            if joinedPeers.count == 200 { break }
        }
        #expect(joinedPeers == (3...202).map { "p\($0)" })
    }
}
