@TestOn('vm')
@Tags(['integration'])
@Timeout(Duration(seconds: 90))
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:math';
import 'dart:typed_data';

import 'package:openwatchparty/openwatchparty.dart';
import 'package:test/test.dart';
import 'package:web_socket/web_socket.dart';

import 'support/fake_player.dart';
import 'support/fake_relay.dart';
import 'support/test_relay.dart';

// These tests run whole parties against owp-relay built from relay/, with relay timers at a tenth of their duration.
// The test of a message over 16384 bytes runs against a local server instead, since no relay sends one.

const media = Media(kind: MediaKind.movie, title: 'Heat', year: 1995, ids: {'imdb': 'tt0113277'}, durationMs: 600000);

void main() {
  test('create, join with a typed code, and join with create after the end', () async {
    final relays = Relays.at((await startRelay()).baseUrl);
    final a = newParty(relays, 'Anna');
    final b = newParty(relays, 'Ben');
    final eventsA = record(a);

    final code = await a.create();
    expect(code, matches(RegExp(r'^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{6}$')));
    expect((a.state.connection, a.state.isHost), (Connection.connected, true));

    await b.join(formatCode(code).toLowerCase());
    expect((b.state.connection, b.state.isHost), (Connection.connected, false));
    expect([for (final p in b.state.room!.participants) p.name], ['Anna', 'Ben']);
    await until(() => eventsA.whereType<ParticipantJoinedEvent>().any((e) => e.participant.name == 'Ben'),
        'Anna told that Ben joined');

    // Anna ends the room, then brings the group back to its code: her join with create makes the room again.
    a.end();
    await until(() => a.state.connection == Connection.ended, 'Anna saw the room end');
    await a.join(formatCode(code).toLowerCase(), create: true);
    expect((a.state.connection, a.state.isHost, a.state.room!.code), (Connection.connected, true, code));
    expect([for (final p in a.state.room!.participants) p.name], ['Anna']);
  });

  test('setMedia: the other party matches, both report ready, the room plays and both players follow', () async {
    final relays = Relays.at((await startRelay()).baseUrl);
    final (:a, :b, :pa, :pb) = await watching(relays);
    expect(b.state.match, const Match(MatchState.matched, 0));
    expect(b.state.choice!.ref, 'b-file');
    await until(() => a.state.room!.participants.every((p) => p.player == PlayerState.ready), 'both ready');
    await Future<void>.delayed(const Duration(seconds: 1));
    expect(pb.positionMs, greaterThan(500));
    expectInSync(a, [pa, pb]);
  });

  test('pause, seek and setRate reach the other player', () async {
    final relays = Relays.at((await startRelay()).baseUrl);
    final (:a, :b, pa: _, :pb) = await watching(relays);

    a.pause();
    await until(() => b.state.room!.phase == Phase.paused && pb.paused, 'Ben paused');
    final pausedAt = b.state.room!.positionMs;
    await until(() => (pb.positionMs - pausedAt).abs() <= 500, 'Ben at the pause position');

    a.seek(120000);
    await until(() => b.state.room!.positionMs == 120000 && pb.positionMs == 120000, 'Ben at 2:00');

    a.setRate(1.5);
    await until(() => b.state.room!.rate == 1.5 && pb.rate == 1.5, 'Ben at 1.5x');
  });

  test('a dropped connection is resumed', () async {
    final relay = await startRelay();
    final proxyPort = TestRelay.nextPort();
    final proxy = await TcpProxy.start(proxyPort, relay.port);
    addTearDown(proxy.close);
    // Anna connects to the relay directly and Ben through the proxy, so cutting the proxy drops only Ben.
    final (:a, :b, :pa, :pb) = await watching(Relays.at(relay.baseUrl), Relays.at('http://127.0.0.1:$proxyPort'));
    final ben = b.state.peerId!;

    proxy.cut();
    await until(() => b.state.connection == Connection.reconnecting, 'Ben reconnecting');
    await until(() => a.state.room!.participant(ben)?.connected == false, 'the relay saw Ben drop');

    await until(() => b.state.connection == Connection.connected, 'Ben resumed', timeout: const Duration(seconds: 20));
    expect(b.state.peerId, ben);
    await until(() => a.state.room!.participant(ben)!.connected, 'the relay saw Ben resume');
    await until(() => inSync(a, [pa, pb]), () => 'in sync after the resume: ${positions(a, [pa, pb])}');

    a.pause();
    await until(() => pb.paused, 'Ben following a pause after the resume');
  });

  test('a relay message of 32768 bytes and more is accepted', () async {
    // A relay holds at most 32 participants, so its messages stay under 32768 bytes (SPEC.md §13.2), but a later v1
    // relay may add members. This server answers join with a joined that carries an unknown member of 40000 bytes.
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, TestRelay.nextPort());
    addTearDown(() => server.close(force: true));
    server.transform(WebSocketTransformer()).listen((socket) {
      socket.listen((Object? text) {
        final message = jsonDecode(text! as String) as Map<String, Object?>;
        switch (message['type']) {
          case 'ping':
            socket.add(jsonEncode({'type': 'pong', 'id': message['id'], 'serverTime': 0}));
          case 'join':
            socket.add(jsonEncode({...joined(), 'notes': 'x' * 40000}));
        }
      });
    });
    final sizes = <int>[];
    final party = newParty(Relays.at('http://127.0.0.1:${server.port}'), 'Ben',
        connect: (url) async => _Measured(await WebSocket.connect(url), sizes));
    await party.join('K7MQ2X');
    expect(sizes.reduce(max), greaterThanOrEqualTo(32768));
    expect(party.state.room!.participants, hasLength(2));
  });
}

Future<TestRelay> startRelay() async {
  final relay = await TestRelay.start(TestRelay.nextPort());
  addTearDown(relay.stop);
  return relay;
}

/// Anna creates a room on [relaysA] and plays [media]; Ben joins on [relaysB], by default the same, matches and opens
/// his file. Returns once both play.
Future<({WatchParty<String> a, WatchParty<String> b, FakePlayer pa, FakePlayer pb})> watching(Relays relaysA,
    [Relays? relaysB]) async {
  final a = newParty(relaysA, 'Anna');
  final b = newParty(relaysB ?? relaysA, 'Ben',
      resolve: (_) => [const Candidate(id: 'b', ref: 'b-file', durationMs: 600000)]);
  final pa = FakePlayer();
  final pb = FakePlayer();
  openChoices(b, pb);
  final code = await a.create();
  await b.join(code);
  await settle();
  a.setMedia(media, 'a-file');
  a.attachPlayer(pa, 'a-file');
  await until(() => a.state.room!.phase == Phase.playing && b.state.room!.phase == Phase.playing, 'the room playing');
  await until(() => !pa.paused && !pb.paused, 'both players playing');
  return (a: a, b: b, pa: pa, pb: pb);
}

/// Lets the relay take every participant's first `status` before the next command. An `idle` status that reaches
/// the relay after a first `setMedia` released the room would pause it (SPEC.md §12.5, nobody in the player).
Future<void> settle() => Future<void>.delayed(const Duration(milliseconds: 300));

WatchParty<String> newParty(Relays relays, String name,
    {List<Candidate<String>> Function(Media media)? resolve, WebSocketConnector? connect}) {
  final party = WatchParty<String>(
    relays: relays,
    client: 'OwpDartTest/1.0.0',
    name: name,
    resolver: _FunctionResolver(resolve ?? (_) => const []),
    connect: connect,
  );
  addTearDown(party.close);
  return party;
}

final class _FunctionResolver implements Resolver<String> {
  const _FunctionResolver(this._resolve);

  final List<Candidate<String>> Function(Media media) _resolve;

  @override
  Future<List<Candidate<String>>> resolve(Media media, Cancellation cancel) async => _resolve(media);
}

/// A WebSocket that records the UTF-8 size of every text message it receives.
final class _Measured implements WebSocket {
  _Measured(this._socket, this._sizes);

  final WebSocket _socket;
  final List<int> _sizes;

  @override
  Stream<WebSocketEvent> get events => _socket.events.map((event) {
        if (event case TextDataReceived(:final text)) _sizes.add(utf8.encode(text).length);
        return event;
      });

  @override
  String get protocol => _socket.protocol;

  @override
  void sendText(String s) => _socket.sendText(s);

  @override
  void sendBytes(Uint8List b) => _socket.sendBytes(b);

  @override
  Future<void> close([int? code, String? reason]) => _socket.close(code, reason);
}

/// What an app does: open each new choice and attach its player.
void openChoices(WatchParty<String> party, FakePlayer player) {
  Candidate<String>? opened;
  party.states.listen((state) {
    final choice = state.choice;
    if (choice == null || identical(choice, opened)) return;
    opened = choice;
    party.attachPlayer(player, choice.ref);
  });
}

List<PartyEvent> record(WatchParty<String> party) {
  final events = <PartyEvent>[];
  party.events.listen(events.add);
  return events;
}

/// Whether every player is within 400 ms of the room position: the SDK leaves drift up to its 350 ms deadband alone,
/// plus 50 ms for reading the positions one after the other. These players play at offset 0.
bool inSync(WatchParty<String> party, List<FakePlayer> players) {
  final room = party.roomPositionMs()!;
  return players.every((player) => (player.positionMs - room).abs() < 400);
}

void expectInSync(WatchParty<String> party, List<FakePlayer> players) {
  expect(inSync(party, players), isTrue, reason: positions(party, players));
}

String positions(WatchParty<String> party, List<FakePlayer> players) =>
    'room at ${party.roomPositionMs()}, players at ${[for (final p in players) '${p.positionMs} × ${p.rate}']}';

/// Waits for [condition]; [what] is a description, or a function that describes the state at the timeout.
Future<void> until(bool Function() condition, Object what, {Duration timeout = const Duration(seconds: 10)}) async {
  final waited = Stopwatch()..start();
  while (!condition()) {
    if (waited.elapsed > timeout) fail('timed out waiting for ${what is String Function() ? what() : what}');
    await Future<void>.delayed(const Duration(milliseconds: 20));
  }
}
