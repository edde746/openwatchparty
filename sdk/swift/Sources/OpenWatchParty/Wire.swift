/// Client → relay messages (SPEC.md §7), as JSON text.
enum Out {
    private static func message(_ type: String, _ fields: (inout JSONObjectWriter) -> Void = { _ in }) -> String {
        var w = JSONObjectWriter()
        w.put("type", type)
        fields(&w)
        return w.done()
    }

    static func create(name: String, client: String) -> String {
        message("create") {
            $0.put("name", name)
            $0.put("client", client)
        }
    }

    static func join(code: String, name: String, client: String, create: Bool) -> String {
        message("join") {
            $0.put("code", code)
            $0.put("name", name)
            $0.put("client", client)
            if create { $0.put("create", true) }
        }
    }

    static func resume(code: String, peerId: String, token: String) -> String {
        message("resume") {
            $0.put("code", code)
            $0.put("peerId", peerId)
            $0.put("token", token)
        }
    }

    static func ping(id: Int64) -> String { message("ping") { $0.put("id", id) } }

    static func status(_ s: StatusFields) -> String {
        message("status") {
            $0.put("epoch", s.epoch)
            $0.put("player", s.player.rawValue)
            if let match = s.match { $0.putRaw("match", match.wireText) } else { $0.putNull("match") }
            $0.put("offsetMs", s.offsetMs)
            if let rtt = s.rttMs { $0.put("rttMs", rtt) }
        }
    }

    static func play() -> String { message("play") }

    static func pause(positionMs: Int64?) -> String {
        message("pause") { if let positionMs { $0.put("positionMs", positionMs) } }
    }

    static func seek(positionMs: Int64) -> String { message("seek") { $0.put("positionMs", positionMs) } }

    static func rate(_ rate: Double) -> String { message("rate") { $0.put("rate", rate) } }

    static func setMedia(_ media: Media, positionMs: Int64) -> String {
        message("setMedia") {
            $0.putRaw("media", media.wireText)
            $0.put("positionMs", positionMs)
        }
    }

    static func clearMedia() -> String { message("clearMedia") }

    static func setControl(_ mode: Control) -> String { message("setControl") { $0.put("mode", mode.rawValue) } }

    static func transferHost(_ peerId: String) -> String { message("transferHost") { $0.put("peerId", peerId) } }

    static func kick(_ peerId: String) -> String { message("kick") { $0.put("peerId", peerId) } }

    static func lock(_ locked: Bool) -> String { message("lock") { $0.put("locked", locked) } }

    static func leave() -> String { message("leave") }

    static func end() -> String { message("end") }
}

/// The fields of one `status` (SDK.md §10.1).
struct StatusFields: Equatable {
    var epoch: Int64
    var player: PlayerState
    var match: Match?
    var offsetMs: Int64
    var rttMs: Int64?
}

/// Relay → client messages (SPEC.md §8) the SDK reads, checked against their schemas (SDK.md §8).
enum In {
    case joined(Joined)
    /// A `room`: the snapshot, the action of the event that published it (SPEC.md §9.1), and its `media` member as
    /// received, which the party checks only when the snapshot starts a new epoch (SDK.md §8).
    case room(Room, action: Action?, media: JSON?)
    case pong(id: Int64, serverTime: Int64)
    /// `message` is the relay's text, for logs only.
    case error(code: String, message: String, re: String?)
    case closed(reason: String)
    /// A `joined` that fails its schema or the descriptor checks: a relay fault (SDK.md §8).
    case malformedJoined

    struct Joined {
        let code: String
        let peerId: String
        let token: String
        let room: Room
        let media: Media?
    }

    /// What happened, and who caused it: nil for the relay.
    struct Action {
        let kind: String
        let by: String?
    }

    /// Nil for a message of unknown type, or one that fails its schema; a malformed `room` is dropped this way, also
    /// for a malformed `action`. A malformed `joined` is `malformedJoined`.
    static func parse(_ text: String) -> In? {
        guard let doc = JSON.parse(text), let o = doc.object, let type = o["type"]?.string else { return nil }
        switch type {
        case "joined":
            return joined(o).map(In.joined) ?? .malformedJoined
        case "room":
            return room(doc).map { .room($0.room, action: $0.action, media: o["media"]) }
        case "pong":
            // Only this SDK's own integer ids can be pending; a string id is someone else's.
            guard let id = o["id"]?.int, let serverTime = o["serverTime"]?.int, serverTime >= 0 else { return nil }
            return .pong(id: id, serverTime: serverTime)
        case "error":
            guard let reJSON = o["re"], let code = o["code"]?.string, !code.isEmpty,
                  let message = o["message"]?.string else { return nil }
            var re: String?
            if !reJSON.isNull {
                guard let s = reJSON.string else { return nil }
                re = s
            }
            return .error(code: code, message: message, re: re)
        case "closed":
            guard let reason = o["reason"]?.string, !reason.isEmpty else { return nil }
            return .closed(reason: reason)
        default:
            return nil
        }
    }

    /// A `media` member: `.some(nil)` for null, the descriptor when it passes the receive checks, nil when it is
    /// neither.
    static func descriptor(_ json: JSON) -> Media?? {
        if json.isNull { return .some(nil) }
        guard let o = json.object, let media = receivedMedia(o) else { return nil }
        return .some(media)
    }

    /// The snapshot and `action` of a `room` message (SDK.md §8); nil when either is malformed. `action`, when present,
    /// is an object with a non-empty `kind` and `by` null or a peer ID. Its `type` and `media` members aren't read.
    static func room(_ json: JSON) -> (room: Room, action: Action?)? {
        guard let room = Room(json: json) else { return nil }
        guard let actionJSON = json["action"] else { return (room, nil) }
        guard let a = actionJSON.object, let byJSON = a["by"], let kind = a["kind"]?.string, !kind.isEmpty else {
            return nil
        }
        if byJSON.isNull { return (room, Action(kind: kind, by: nil)) }
        guard let by = byJSON.string, isPeerId(by) else { return nil }
        return (room, Action(kind: kind, by: by))
    }

    private static func joined(_ o: [String: JSON]) -> Joined? {
        guard let you = o["you"]?.object,
              let serverTime = o["serverTime"]?.int, serverTime >= 0,
              let mediaJSON = o["media"],
              let code = o["code"]?.string, isRoomCode(code),
              let peerId = you["peerId"]?.string, isPeerId(peerId),
              let token = you["token"]?.string, isToken(token),
              let room = Room(json: o["room"]),
              let media = descriptor(mediaJSON) else { return nil }
        return Joined(code: code, peerId: peerId, token: token, room: room, media: media)
    }
}
