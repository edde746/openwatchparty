import { Relays, WatchParty, attachVideo, formatCode, sameTitle } from "openwatchparty";

const $ = (id) => document.getElementById(id);
const video = $("video");
const CLIENT = "owp-web-example/0.1.0";

let file = null; // the File the <video> plays
let party = null; // the current WatchParty
let detach = null; // detaches the <video> from the party
let opened = null; // the latest choice the page has acted on

// One relay list per source, kept for the page's life.
const lists = new Map();
function relaysFor(base) {
  if (!lists.has(base)) lists.set(base, base ? Relays.at(base) : Relays.public());
  return lists.get(base);
}

$("relay").value = new URLSearchParams(location.search).get("relay") ?? "";

const say = (text) => ($("log").textContent = text);
const seconds = (ms) => `${(ms / 1000).toFixed(1)} s`;
const signed = (ms) => (ms > 0 ? "+" : ms < 0 ? "−" : "") + seconds(Math.abs(ms));

// The page's own words for each error code and each way out of the room. An error's `message` is for logs only.
const TEXT = {
  unreachable: "Can't reach the relay. Check your connection and try again.",
  relay_full: "The relays are full right now. Try again in a few minutes.",
  room_not_found: "There is no room with this code. Check the code and try again.",
  room_full: "This room is full.",
  room_locked: "The host has locked this room.",
  invalid_code: "A room code has 6 letters and digits, like K7M-Q2X.",
  rate_limited: "Too many tries at once. Wait a moment and try again.",
  left: "You left the room.",
  ended: "The room has ended.",
  kicked: "The host removed you from the room.",
  expired: "The room reached its time limit and closed.",
  replaced: "This room continues in another window.",
  lost: "Lost the connection to the room.",
};
const problem = (e, invalid = "That value isn't accepted.") =>
  e?.code === "invalid_message" ? invalid : (TEXT[e?.code] ?? "Something went wrong. Try again.");

// The descriptor of the picked file. A TMDB ID makes it a movie, matched by ID; without one it is a plain video,
// matched by file name and size.
function describe() {
  const tmdb = $("tmdb").value.trim();
  const media = {
    kind: tmdb ? "movie" : "video",
    title: $("title").value.trim() || file.name,
    durationMs: Math.round(video.duration * 1000),
    file: { name: file.name, sizeBytes: file.size },
  };
  if (tmdb) media.ids = { tmdb };
  return media;
}

// The picked file is the only candidate: for a movie when the title matches, for a video when it's the same file.
function found(media) {
  if (file === null || !Number.isFinite(video.duration)) return [];
  const own = describe();
  const sameFile = media.file?.name === file.name && media.file?.sizeBytes === file.size;
  if (media.kind === "video" ? !sameFile : !sameTitle(media, own)) return [];
  return [{ id: file.name, ref: file, durationMs: own.durationMs, name: file.name, sizeBytes: file.size }];
}

// Attaches the <video> with the File it plays as the ref; the SDK follows the room whenever that File is the choice.
function attach() {
  detach?.();
  detach = party === null ? null : attachVideo(party, video, file);
}

// Plays the File `f` in the <video> and calls `then` once it is open. A newer load replaces an older one's `then`.
function load(f, then) {
  file = f;
  if (video.src.startsWith("blob:")) URL.revokeObjectURL(video.src);
  video.onloadedmetadata = () => ((video.onloadedmetadata = null), then());
  video.src = URL.createObjectURL(f);
}

// Host: share the open file with the room. Its File is the ref, so the room's media comes back as a choice of this
// file, and the host finds the file there again after handing host to someone else.
function share() {
  if (file === null) return say("Pick a video file to share it with the room.");
  try {
    party.setMedia(describe(), file, Math.floor(video.currentTime * 1000));
  } catch (e) {
    say(problem(e, "This file can't be shared as it is. Shorten or change the title."));
  }
}

// The picked file or its title changed: the host shares it again; a guest searches again, or with a file already
// chosen, plays the new one if it fits.
function mediaChanged() {
  const s = party?.state;
  if (s?.connection !== "connected" || file === null) return;
  if (s.isHost) return share();
  if (s.choice === null) return party.resolveAgain();
  const [candidate] = found(s.media);
  if (candidate !== undefined) void party.choose(candidate);
}

async function start(create) {
  detach?.();
  party?.close();
  detach = opened = party = null;
  let hint = "Enter a relay address like https://relay.example.com, or leave it empty.";
  try {
    const relays = relaysFor($("relay").value.trim());
    hint = "Enter a name of 1 to 32 characters.";
    party = new WatchParty({ relays, client: CLIENT, name: $("name").value.trim(), resolver: { resolve: found } });
  } catch (e) {
    return say(problem(e, hint));
  }
  party.on("state", render);
  party.on("event", onEvent);
  say(create ? "Creating a room…" : "Joining (or reopening a saved room)…");
  const own = party;
  try {
    if (create) await own.create();
    else await own.join($("code").value);
    say(create ? "Room created. Save this code to reuse it." : "Joined. Saved room codes can be reused.");
    if (create) share();
  } catch (e) {
    if (own === party) say(problem(e)); // else the user started another session
  }
}

function render(s) {
  // Open the SDK's choice for the room's media and attach; a <video> that already plays it isn't reloaded.
  if (s.choice !== opened) {
    opened = s.choice;
    if (opened !== null && opened.ref !== file) load(opened.ref, attach);
    else if (opened !== null && detach === null) attach();
  }
  const room = s.room;
  $("room").hidden = room === null || s.connection === "ended";
  if (room === null) return;
  $("code-display").textContent = formatCode(room.code);
  $("status").textContent = `${s.connection}, ${room.phase}${s.isHost ? ", you are the host" : ""}`;
  const prompt = $("mismatch");
  const asking = s.mismatch !== null;
  if (asking) {
    const delta = s.mismatch.deltaMs;
    $("mismatch-text").textContent = `Your file is ${seconds(Math.abs(delta))} ${delta > 0 ? "longer" : "shorter"} ` +
      "than the shared one. Play it anyway? If it starts at a different point, set an offset below.";
  }
  if (asking && prompt.hidden) queueMicrotask(() => $("accept").focus());
  prompt.hidden = !asking;
  // The play prompt of playBlocked stays until the video plays or the room stops playing.
  $("blocked").hidden ||= room.phase !== "playing" || !video.paused;
  if (document.activeElement !== $("offset")) $("offset").value = String(s.offsetMs);
  $("people").replaceChildren(...room.participants.map((p) => person(p, s.peerId)));
}

function person(p, me) {
  const m = p.match;
  const match = m === null ? "no media" : m.state + (m.deltaMs ? ` (file ${signed(m.deltaMs)})` : "");
  const parts = [p.name + (p.peerId === me ? " (you)" : ""), p.host && "host", p.connected ? p.player : "disconnected",
    match, `offset ${signed(p.offsetMs)}`];
  return Object.assign(document.createElement("li"), { textContent: parts.filter(Boolean).join(" · ") });
}

// Events the page doesn't know (a later SDK may add some) are ignored.
function onEvent(e) {
  if (e.type === "action" && !e.byMe) say(`${e.by?.name ?? "The relay"}: ${e.kind}`);
  if (e.type === "participantJoined") say(`${e.participant.name} joined.`);
  if (e.type === "participantLeft") say(`${e.participant.name} left.`);
  if (e.type === "error") say(problem(e.error));
  if (e.type === "playBlocked") $("blocked").hidden = false;
  if (e.type === "ended") {
    detach?.();
    detach = opened = null;
    say(TEXT[e.reason]);
  }
}

$("file").addEventListener("change", () => {
  const picked = $("file").files[0];
  if (picked === undefined) return;
  detach?.(); // the <video> no longer plays the file it was attached with
  detach = null;
  load(picked, mediaChanged);
});
$("title").addEventListener("change", mediaChanged);
$("tmdb").addEventListener("change", mediaChanged);
$("create").addEventListener("click", () => start(true));
$("join-form").addEventListener("submit", (e) => (e.preventDefault(), start(false)));
$("leave").addEventListener("click", () => party?.leave());
$("accept").addEventListener("click", () => party?.acceptMismatch());
$("decline").addEventListener("click", () => party?.declineMismatch());
$("offset-form").addEventListener("submit", (e) => {
  e.preventDefault();
  try {
    party?.setOffset(Math.round(Number($("offset").value)));
  } catch (err) {
    say(problem(err, "An offset is at most 7 days either way."));
  }
});
