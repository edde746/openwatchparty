package party.openwatch

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonObjectBuilder
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put

/** Client → relay messages (SPEC.md §7), as JSON text. */
internal object Out {
    private fun message(type: String, fields: JsonObjectBuilder.() -> Unit = {}): String =
        buildJsonObject {
            put("type", type)
            fields()
        }.toString()

    fun create(name: String, client: String) = message("create") {
        put("name", name)
        put("client", client)
    }

    fun join(code: String, name: String, client: String, create: Boolean) = message("join") {
        put("code", code)
        put("name", name)
        put("client", client)
        if (create) put("create", true)
    }

    fun resume(code: String, peerId: String, token: String) = message("resume") {
        put("code", code)
        put("peerId", peerId)
        put("token", token)
    }

    fun ping(id: Long) = message("ping") { put("id", id) }

    fun status(s: StatusFields) = message("status") {
        put("epoch", s.epoch)
        put("player", s.player.wire)
        put("match", s.match?.toJson() ?: JsonNull)
        put("offsetMs", s.offsetMs)
        s.rttMs?.let { put("rttMs", it) }
    }

    fun play() = message("play")

    fun pause(positionMs: Long?) = message("pause") { positionMs?.let { put("positionMs", it) } }

    fun seek(positionMs: Long) = message("seek") { put("positionMs", positionMs) }

    fun rate(rate: Double) = message("rate") { put("rate", rate) }

    fun setMedia(media: JsonObject, positionMs: Long) = message("setMedia") {
        put("media", media)
        put("positionMs", positionMs)
    }

    fun clearMedia() = message("clearMedia")

    fun setControl(mode: Control) = message("setControl") { put("mode", mode.wire) }

    fun transferHost(peerId: String) = message("transferHost") { put("peerId", peerId) }

    fun kick(peerId: String) = message("kick") { put("peerId", peerId) }

    fun lock(locked: Boolean) = message("lock") { put("locked", locked) }

    fun leave() = message("leave")

    fun end() = message("end")
}

/** The fields of one `status` (SDK.md §10.1). */
internal data class StatusFields(
    val epoch: Long,
    val player: PlayerState,
    val match: Match?,
    val offsetMs: Long,
    val rttMs: Long?,
)

/** Relay → client messages (SPEC.md §8) the SDK reads, checked against their schemas (SDK.md §8). */
internal sealed interface In {
    class Joined(val code: String, val peerId: String, val token: String, val room: Room, val media: Media?) : In

    /**
     * A `room` (SPEC.md §8): the snapshot, the [action] that produced it, and its `media` member, which counts only on
     * a snapshot of a new epoch (SDK.md §8): [carriesMedia] when it is present and null or a valid descriptor, which is
     * then [media].
     */
    class RoomMsg(val room: Room, val action: Action?, val carriesMedia: Boolean, val media: Media?) : In

    /** The action a `room` carries; [by] is null for the relay. */
    class Action(val kind: String, val by: String?)

    class Pong(val id: Long, val serverTime: Long) : In

    /** [message] is the relay's text, for logs only. */
    class Error(val code: String, val message: String, val re: String?) : In

    class Closed(val reason: String) : In

    /** A `joined` that fails its schema or the descriptor checks: a relay fault (SDK.md §8). */
    data object MalformedJoined : In

    companion object {
        private val TOKEN = Regex("^[A-Za-z0-9_-]{43}$")

        /** A resume token as SPEC.md §11.1 shapes it: 43 base64url characters. */
        fun isToken(s: String): Boolean = TOKEN.matches(s)

        /**
         * Null for a message of unknown type, or one that fails its schema; a malformed `room` is dropped this way.
         * A malformed `joined` is [MalformedJoined].
         */
        fun parse(text: String): In? {
            val o = try {
                Json.parseToJsonElement(text).asObject()
            } catch (e: Exception) {
                null
            } ?: return null
            return when (o["type"].asString()) {
                "joined" -> joined(o) ?: MalformedJoined
                "room" -> room(o)
                // Only this SDK's own integer ids can be pending; a string id is someone else's.
                "pong" -> Pong(o["id"].asInt() ?: return null, o["serverTime"].asInt()?.takeIf { it >= 0 } ?: return null)
                "error" -> {
                    val re = o["re"] ?: return null
                    Error(
                        code = o["code"].asString()?.takeIf { it.isNotEmpty() } ?: return null,
                        message = o["message"].asString() ?: return null,
                        re = if (re is JsonNull) null else (re.asString() ?: return null),
                    )
                }
                "closed" -> Closed(o["reason"].asString()?.takeIf { it.isNotEmpty() } ?: return null)
                else -> null
            }
        }

        private fun joined(o: JsonObject): Joined? {
            val you = o["you"].asObject() ?: return null
            if (o["serverTime"].asInt()?.takeIf { it >= 0 } == null) return null
            val media = o["media"] ?: return null
            return Joined(
                code = o["code"].asString()?.takeIf { Room.isRoomCode(it) } ?: return null,
                peerId = you["peerId"].asString()?.takeIf { Room.isPeerId(it) } ?: return null,
                token = you["token"].asString()?.takeIf(::isToken) ?: return null,
                room = Room.fromJson(o["room"]) ?: return null,
                media = if (media is JsonNull) null else (media.asObject()?.let(::receivedMedia) ?: return null),
            )
        }

        /**
         * The members of a `room` as SDK.md §8 checks them: null unless its snapshot passes and its `action`, when
         * present, is `{kind, by}` with a non-empty `kind` and `by` null or a peer ID.
         */
        fun room(o: JsonObject): RoomMsg? {
            val room = Room.fromJson(o) ?: return null
            val action = o["action"]?.let { a ->
                val ao = a.asObject() ?: return null
                val by = ao["by"] ?: return null
                Action(
                    kind = ao["kind"].asString()?.takeIf { it.isNotEmpty() } ?: return null,
                    by = if (by is JsonNull) null else (by.asString()?.takeIf { Room.isPeerId(it) } ?: return null),
                )
            }
            val media = o["media"]
            val descriptor = media.asObject()?.let(::receivedMedia)
            return RoomMsg(room, action, carriesMedia = media is JsonNull || descriptor != null, media = descriptor)
        }
    }
}
