import Foundation
import Testing
@testable import OpenWatchParty

/// What the SDK sends (SDK.md §2, §7.1) and its timers (§2).
@MainActor
@Suite(.timeLimit(.minutes(1)))
struct WireTests {
    @Test func intsGoOutWithoutAFractionOrExponent() throws {
        let max: Int64 = 9_007_199_254_740_991
        #expect(JSONValue.number(Double(max)).description == "9007199254740991")
        #expect(JSONValue.number(-Double(max)).description == "-9007199254740991")
        #expect(JSONValue.number(604_800_000).description == "604800000")
        // Positions computed with doubles still go out as ints: (11001 - 1000) / 1.25 = 8000.8.
        let position = Int64((Double(11_001 - 1_000) / 1.25).rounded(.down))
        #expect(Out.seek(positionMs: position) == #"{"type":"seek","positionMs":8000}"#)
        let text = Out.status(StatusFields(epoch: max, player: .ready, match: Match(.matched, deltaMs: -1_203), offsetMs: -604_800_000, rttMs: 42))
        #expect(!text.contains(".") && !text.contains("e+") && !text.contains("E"), "\(text)")
        #expect(text.contains(#""epoch":9007199254740991"#))
        #expect(text.contains(#""offsetMs":-604800000"#))
        var media = movie
        media.durationMs = 604_800_000
        media.year = 1995
        media.file = MediaFile(sizeBytes: 4_700_000_000)
        let sent = Out.setMedia(media, positionMs: 3_600_000)
        #expect(sent.contains(#""durationMs":604800000"#) && sent.contains(#""sizeBytes":4700000000"#), "\(sent)")
        #expect(sent.contains(#""positionMs":3600000"#) && sent.contains(#""year":1995"#), "\(sent)")
    }

    @Test func theEncoderNeverRepeatsAMemberName() {
        var media = movie
        media.ext = ["com.example": ["a": 1, "b": [true, nil, "x"]]]
        for text in [
            Out.create(name: "Ben", client: "Test/1.0"), Out.join(code: "K7MQ2X", name: "Ben", client: "Test/1.0", create: true),
            Out.setMedia(media, positionMs: 0), Out.pause(positionMs: nil), Out.pause(positionMs: 5),
            Out.status(StatusFields(epoch: 1, player: .idle, match: nil, offsetMs: 0, rttMs: nil)),
        ] {
            expectNoRepeatedNames(text)
        }
    }

    @Test func aTimerLongerThan2147483647MsRunsInSeveralWaitsAndNeverEarly() async {
        let clock = VirtualClock()
        let fired = Box(0)
        let delay: Int64 = 5_000_000_000
        _ = clock.after(delay) { fired.value += 1 }
        #expect(clock.requested == [Owp.maxTimerMs], "no native timer is longer")
        await clock.advance(by: Owp.maxTimerMs)
        #expect(fired.value == 0)
        #expect(clock.requested == [Owp.maxTimerMs, Owp.maxTimerMs], "re-armed once for what is left, capped")
        await clock.advance(by: Owp.maxTimerMs)
        #expect(fired.value == 0)
        #expect(clock.requested.last == delay - 2 * Owp.maxTimerMs)
        await clock.advance(by: delay - 2 * Owp.maxTimerMs - 1)
        #expect(fired.value == 0, "not early")
        await clock.advance(by: 1)
        #expect(fired.value == 1)
        #expect(clock.requested.count == 3, "no tight re-arming loop")
    }

    @Test func aCancelledAlarmNeverRuns() async {
        let clock = VirtualClock()
        let fired = Box(false)
        let alarm = clock.after(3_000_000_000) { fired.value = true }
        await clock.advance(by: Owp.maxTimerMs)
        alarm.cancel()
        await clock.advance(by: 3_000_000_000)
        #expect(!fired.value)
    }
}
