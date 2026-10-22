@TestOn('vm')
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:math';
import 'dart:typed_data';

import 'package:fake_async/fake_async.dart';
import 'package:openwatchparty/openwatchparty.dart';
import 'package:test/test.dart';
import 'package:web_socket/web_socket.dart';

// Runs every trace of conformance/vectors/party.json (SDK.md §17): a real WatchParty against a scripted relay,
// scripted adapters and a scripted resolver, in virtual time. conformance/vectors/README.md (the party section) defines
// the format; this file is its Dart driver.

typedef Json = Map<String, Object?>;

const _channels = ['send', 'command', 'event', 'resolver', 'net', 'done'];

void main() {
  final document = jsonDecode(File('../../conformance/vectors/party.json').readAsStringSync()) as Json;
  group('party', () {
    for (final trace in (document['cases'] as List).cast<Json>()) {
      test(trace['name'] as String, () => fakeAsync((async) => _runTrace(trace, async)));
    }
  });
}

void _runTrace(Json trace, FakeAsync async) {
  final input = trace['input'] as Json;
  final steps = (input['steps'] as List).cast<Json>();
  final outputs = ((trace['expect'] as Json)['outputs'] as List).cast<Json>();
  final run = _Run(input['setup'] as Json, async);
  try {
    run.settle();
    for (final (i, step) in steps.indexed) {
      run.step(step);
      final want = _expected(outputs[i]);
      final got = run.take(want);
      final where = 'step $i ${jsonEncode(step)} at local ${run.now}';
      expect(run.problems, isEmpty, reason: where);
      expect(got, equals(want), reason: where);
      expect(run.draws.missing, isFalse, reason: 'step $i: the party drew more random values than setup.random holds');
    }
    expect(run.draws.left, 0, reason: 'setup.random holds draws the party never took');
    expect(run.resolving, isNull, reason: 'a resolve call is still unanswered');
    expect(run.probes.keys, isEmpty, reason: 'a probe is still unanswered');
    for (final player in run.players.values) {
      expect(player.held, isNull, reason: '${player.id} still runs a held command');
    }
    final answered = {
      for (final output in outputs)
        for (final done in (output['done'] as List?) ?? const []) (done as Json)['id'],
    };
    for (final id in run.calls) {
      expect(answered, contains(id), reason: 'call $id never settled');
    }
  } finally {
    unawaited(run.party.close());
    run.settle();
  }
}

/// The randomness seam: each `nextDouble()` is the next scripted u (SDK.md §16.8).
final class _Draws implements Random {
  _Draws(this._values);

  final List<double> _values;
  var missing = false;
  var misused = false;

  int get left => _values.length;

  @override
  double nextDouble() {
    if (_values.isEmpty) {
      missing = true;
      return 0;
    }
    return _values.removeAt(0);
  }

  @override
  int nextInt(int max) {
    misused = true;
    return 0;
  }

  @override
  bool nextBool() {
    misused = true;
    return false;
  }
}

/// A relay connection: pings are answered by the driver, everything else the party sends is an output.
final class _Socket implements WebSocket {
  _Socket(this._run, this.url);

  final _Run _run;
  final Uri url;
  final _events = StreamController<WebSocketEvent>();
  var opened = false;
  var _closed = false;

  bool get isOpen => opened && !_closed;

  @override
  Stream<WebSocketEvent> get events => _events.stream;

  @override
  String get protocol => '';

  @override
  void sendText(String s) {
    if (_closed) throw WebSocketConnectionClosed();
    final message = jsonDecode(s) as Json;
    if (message['type'] == 'ping') {
      _run.pong(this, message['id']);
    } else {
      _run.record('send', {'on': '$url', 'msg': message});
    }
  }

  @override
  void sendBytes(Uint8List b) {
    if (_closed) throw WebSocketConnectionClosed();
    _run.problems.add('the party sent binary data to $url');
  }

  /// The party closing the connection; as `package:web_socket`, a closed one throws.
  @override
  Future<void> close([int? code, String? reason]) async {
    if (_closed) throw WebSocketConnectionClosed();
    _closed = true;
    _run.record('net', {'close': '$url', 'code': code});
    unawaited(_events.close());
  }

  void deliver(String text) {
    if (isOpen) _events.add(TextDataReceived(text));
  }

  /// The relay closing the connection, with [code] or without a status.
  void drop(int? code) {
    _closed = true;
    _events.add(CloseReceived(code));
    unawaited(_events.close());
  }
}

/// A command left running by a `hold` step: what it does when it finishes, and its completion.
typedef _Held = ({void Function() apply, Completer<void> done});

/// A scripted adapter. Its position moves at the rate last set while it plays; commands take effect when they finish.
final class _Player extends PlayerAdapter {
  _Player(this.id, this._run) : _since = _run.now;

  final String id;
  final _Run _run;
  var _position = 0;
  int _since;
  var _rate = 1.0;
  var _paused = true;
  var _buffering = false;
  int? _durationMs;
  var _firstFrameShown = true;
  var _canSetRate = false;
  var _seekAccuracyMs = 0;
  var _playThrows = false;

  /// The next command stays running until a `finish` step.
  var holdNext = false;
  _Held? held;

  @override
  int get positionMs => _paused || _buffering ? _position : _position + ((_run.now - _since) * _rate).floor();

  @override
  bool get paused => _paused;

  @override
  bool get buffering => _buffering;

  @override
  int? get durationMs => _durationMs;

  @override
  bool get firstFrameShown => _firstFrameShown;

  @override
  bool get canSetRate => _canSetRate;

  @override
  int get seekAccuracyMs => _seekAccuracyMs;

  /// Changes the readable state from now on.
  void set(Json fields) {
    _position = positionMs;
    _since = _run.now;
    for (final MapEntry(:key, :value) in fields.entries) {
      switch (key) {
        case 'positionMs':
          _position = value as int;
        case 'paused':
          _paused = value as bool;
        case 'buffering':
          _buffering = value as bool;
        case 'durationMs':
          _durationMs = value as int?;
        case 'firstFrameShown':
          _firstFrameShown = value as bool;
        case 'canSetRate':
          _canSetRate = value as bool;
        case 'seekAccuracyMs':
          _seekAccuracyMs = value as int;
        case 'playThrows':
          _playThrows = value as bool;
        default:
          fail('unknown player member $key');
      }
    }
  }

  @override
  Future<void> play() => _command({'do': 'play'}, () => set(const {'paused': false}), throws: _playThrows);

  @override
  Future<void> pause() => _command({'do': 'pause'}, () => set(const {'paused': true}));

  @override
  Future<void> seek(int positionMs) =>
      _command({'do': 'seek', 'positionMs': positionMs}, () => set({'positionMs': positionMs}));

  @override
  Future<void> setRate(double rate) => _command({'do': 'setRate', 'rate': _round6(rate)}, () {
        set(const {});
        _rate = rate;
      });

  Future<void> _command(Json command, void Function() apply, {bool throws = false}) {
    _run.record('command', {'player': id, ...command});
    if (holdNext) {
      holdNext = false;
      final done = Completer<void>();
      held = (apply: apply, done: done);
      return done.future;
    }
    if (throws) return Future.error(StateError('$id refused to ${command['do']}'));
    apply();
    return Future.value();
  }
}

final class _Resolver implements Resolver<String> {
  _Resolver(this._run);

  final _Run _run;

  @override
  Future<List<Candidate<String>>> resolve(Media media, Cancellation cancel) => _run.resolve(media, cancel);
}

/// A running resolver or probe call: its answer, and the output its cancellation records.
final class _Pending<V> {
  _Pending(this.cancel, this.cancelled);

  final Cancellation cancel;
  final Json cancelled;
  final answer = Completer<V>();
}

/// One trace's world: the party, its fakes and the outputs of the current step.
final class _Run {
  _Run(this.setup, this.async)
      : _startMs = setup['startMs'] as int,
        _rttAll = setup['rttMs'] as int,
        draws = _Draws([for (final u in setup['random'] as List) (u as num).toDouble()]) {
    final base = setup['base'] as String?;
    relays = base == null ? Relays.public(httpGet: _get) : Relays.at(base);
    party = WatchParty<String>(
      relays: relays,
      client: setup['client'] as String,
      name: setup['name'] as String,
      resolver: setup['resolver'] == true ? _Resolver(this) : null,
      connect: _connect,
      nowMs: () => now,
      random: draws,
    );
    party.events.listen((event) => record('event', _projectEvent(event)));
  }

  final Json setup;
  final FakeAsync async;
  final int _startMs;
  final _Draws draws;
  late final Relays relays;
  late final WatchParty<String> party;

  Map<String, List<Object?>> _out = _noOutputs();

  /// What went wrong inside a callback of the party, where throwing would reach the party instead of the test.
  final List<String> problems = [];
  final List<_Socket> sockets = [];
  final Map<String, _Player> players = {};
  final Set<String> calls = {};

  /// How the next connection opens, per URL, and for any URL under ''.
  final Map<String, String> _nextOpen = {};

  /// The round trip of pings sent from now on, per URL, else [_rttAll]; null never answers.
  final Map<String, int?> _rtt = {};
  int? _rttAll;
  Object? _nextGet;
  _Pending<List<Candidate<String>>>? resolving;
  final Map<String, _Pending<int?>> probes = {};

  int get now => _startMs + async.elapsed.inMilliseconds;

  static Map<String, List<Object?>> _noOutputs() => {for (final channel in _channels) channel: []};

  void record(String channel, Object? output) => _out[channel]!.add(output);

  /// Runs everything ready without moving time: continuations, connections opening, timers due now.
  void settle() => async.elapse(Duration.zero);

  Future<WebSocket> _connect(Uri url) {
    final socket = _Socket(this, url);
    sockets.add(socket);
    record('net', {'open': '$url'});
    final key = _nextOpen.containsKey('$url') ? '$url' : '';
    final how = _nextOpen.remove(key) ?? 'open';
    final opening = Completer<WebSocket>();
    if (how == 'open') {
      Timer.run(() {
        socket.opened = true;
        opening.complete(socket);
      });
    } else if (how == 'fail') {
      Timer.run(() => opening.completeError(WebSocketException('$url refused')));
    }
    return opening.future;
  }

  /// A GET of the public list: answered with setup.list unless a nextGet step scripted this one.
  Future<String> _get(Uri url, Cancellation cancel) {
    record('net', {'get': '$url'});
    final how = _nextGet ?? setup['list'];
    _nextGet = null;
    if (how == 'fail') return Future.error(StateError('GET $url failed'));
    if (how == 'hang') return Completer<String>().future;
    return Future.value(jsonEncode(how));
  }

  /// Answers a ping its URL's round trip later, with relay time = send time + floor(rtt / 2) + relayOffsetMs.
  void pong(_Socket socket, Object? id) {
    final url = '${socket.url}';
    final rtt = _rtt.containsKey(url) ? _rtt[url] : _rttAll;
    if (rtt == null) return;
    final serverTime = now + rtt ~/ 2 + (setup['relayOffsetMs'] as int);
    final pong = jsonEncode({'type': 'pong', 'id': id, 'serverTime': serverTime});
    Timer(Duration(milliseconds: rtt), () => socket.deliver(pong));
  }

  /// The newest connection that is still open, to [on] when given.
  _Socket socket(String? on) => sockets.lastWhere((s) => s.isOpen && (on == null || '${s.url}' == on),
      orElse: () => fail('no open connection${on == null ? '' : ' to $on'}'));

  _Player player(String id) => players[id] ?? fail('unknown player $id');

  Future<List<Candidate<String>>> resolve(Media media, Cancellation cancel) {
    _noteCancellations();
    if (resolving != null) problems.add('a second resolve call while one is pending');
    record('resolver', {'resolve': media.title});
    final pending = resolving = _Pending(cancel, {'resolveCancelled': media.title});
    unawaited(cancel.whenCancelled.then((_) => _noteCancellations()));
    return pending.answer.future;
  }

  Future<int?> _probe(String id, Cancellation cancel) {
    _noteCancellations();
    if (probes.containsKey(id)) problems.add('probe $id called twice at once');
    record('resolver', {'probe': id});
    final pending = probes[id] = _Pending(cancel, {'probeCancelled': id});
    unawaited(cancel.whenCancelled.then((_) => _noteCancellations()));
    return pending.answer.future;
  }

  /// Records each running resolver or probe call the party has cancelled, once. A call that answered is done: a
  /// cancellation after its answer cancels nothing.
  void _noteCancellations() {
    final pending = resolving;
    if (pending != null && pending.cancel.isCancelled) {
      resolving = null;
      record('resolver', pending.cancelled);
    }
    for (final id in [...probes.keys]) {
      final probe = probes[id]!;
      if (!probe.cancel.isCancelled) continue;
      probes.remove(id);
      record('resolver', probe.cancelled);
    }
  }

  Candidate<String> candidate(Json c) {
    final id = c['id'] as String;
    return Candidate(
      id: id,
      ref: c['ref'] as String? ?? id,
      durationMs: c['durationMs'] as int?,
      sizeBytes: c['sizeBytes'] as int?,
      oshash: c['oshash'] as String?,
      name: c['name'] as String?,
      probe: c['probe'] == true ? (cancel) => _probe(id, cancel) : null,
    );
  }

  /// Runs one step and settles.
  void step(Json s) {
    if (s.containsKey('call')) {
      _call(s);
    } else if (s.containsKey('recv')) {
      socket(s['on'] as String?).deliver(jsonEncode(s['recv']));
    } else if (s.containsKey('recvText')) {
      socket(s['on'] as String?).deliver(s['recvText'] as String);
    } else if (s.containsKey('drop')) {
      socket(s['on'] as String?).drop(s['drop'] as int?);
    } else if (s.containsKey('nextOpen')) {
      _nextOpen[s['on'] as String? ?? ''] = s['nextOpen'] as String;
    } else if (s.containsKey('rtt')) {
      if (s['on'] case final String on) {
        _rtt[on] = s['rtt'] as int?;
      } else {
        _rttAll = s['rtt'] as int?;
        _rtt.clear();
      }
    } else if (s.containsKey('nextGet')) {
      _nextGet = s['nextGet'];
    } else if (s.containsKey('advance')) {
      async.elapse(Duration(milliseconds: s['advance'] as int));
      return;
    } else if (s.containsKey('player')) {
      final id = s['player'] as String;
      (players[id] ??= _Player(id, this)).set({...s}..remove('player'));
    } else if (s.containsKey('hold')) {
      player(s['hold'] as String).holdNext = true;
    } else if (s.containsKey('finish')) {
      final p = player(s['finish'] as String);
      final held = p.held ?? fail('${p.id} has no running command to finish');
      p.held = null;
      if (s['throws'] == true) {
        held.done.completeError(StateError('${p.id} failed'));
      } else {
        held.apply();
        held.done.complete();
      }
    } else if (s.containsKey('answer') || s.containsKey('answerThrows')) {
      final pending = resolving ?? fail('no resolve call to answer');
      resolving = null;
      if (s['answer'] case final List<Object?> answer) {
        pending.answer.complete([for (final c in answer) candidate(c as Json)]);
      } else {
        pending.answer.completeError(StateError('the resolver failed'));
      }
    } else if (s.containsKey('probeAnswer') || s.containsKey('probeThrows')) {
      final id = (s['probeAnswer'] ?? s['probeThrows']) as String;
      final pending = probes.remove(id) ?? fail('no running probe of $id');
      if (s.containsKey('probeAnswer')) {
        pending.answer.complete(s['durationMs'] as int?);
      } else {
        pending.answer.completeError(StateError('the probe of $id failed'));
      }
    } else {
      fail('unknown step ${jsonEncode(s)}');
    }
    settle();
  }

  void _call(Json s) {
    final call = s['call'] as String;
    final id = s['id'] as String?;
    if (id != null) calls.add(id);
    void ok([Object? value]) {
      if (id != null) record('done', {'id': id, if (value != null) 'value': value});
    }

    void failed(Object error) {
      if (error is! PartyException) {
        problems.add('$call threw $error');
      } else if (id == null) {
        problems.add('$call without an id threw ${error.code}');
      } else {
        record('done', {'id': id, 'error': error.code, 're': error.re, 'message': error.message});
      }
    }

    void later(Future<Object?> future) => unawaited(future.then(ok, onError: failed));

    try {
      switch (call) {
        case 'create':
          return later(party.create());
        case 'join':
          return later(party.join(s['code'] as String, create: s['create'] as bool? ?? false));
        case 'choose':
          return later(party.choose(candidate(s['candidate'] as Json)));
        case 'detachPlayer':
          return later(party.detachPlayer(s['player'] == null ? null : player(s['player'] as String)));
        case 'close':
          return later(party.close());
        case 'refresh':
          return later(relays.refresh());
        case 'attachPlayer':
          party.attachPlayer(player(s['player'] as String), s['ref'] as String);
        case 'setMedia':
          final media = parseMedia(s['media']);
          if (s['positionMs'] case final int positionMs) {
            party.setMedia(media, s['ref'] as String, positionMs: positionMs);
          } else {
            party.setMedia(media, s['ref'] as String);
          }
        case 'clearMedia':
          party.clearMedia();
        case 'play':
          party.play();
        case 'pause':
          party.pause();
        case 'seek':
          party.seek(s['ms'] as int);
        case 'setRate':
          party.setRate((s['rate'] as num).toDouble());
        case 'setOffset':
          party.setOffset(s['ms'] as int);
        case 'setControl':
          party.setControl(Control.values.byName(s['mode'] as String));
        case 'lock':
          party.lock(s['locked'] as bool);
        case 'kick':
          party.kick(s['peerId'] as String);
        case 'transferHost':
          party.transferHost(s['peerId'] as String);
        case 'leave':
          party.leave();
        case 'end':
          party.end();
        case 'acceptMismatch':
          party.acceptMismatch();
        case 'declineMismatch':
          party.declineMismatch();
        case 'resolveAgain':
          party.resolveAgain();
        case 'playerChanged':
          party.playerChanged();
        default:
          fail('unknown call $call');
      }
      ok();
    } on PartyException catch (error) {
      failed(error);
    }
  }

  /// This step's outputs as [want] writes them: a sent message with its connection only where [want] pins it, an
  /// error's `re` and `message` only where [want]'s entry of that call has them, `net` per URL, `done` by id, and the
  /// state fields [want] names.
  Json take(Json want) {
    _noteCancellations();
    if (draws.misused) problems.add('the party drew with nextInt or nextBool, not nextDouble');
    final fields = ((want['state'] as Json?) ?? const {}).keys;
    final got = <String, Object?>{
      for (final channel in _channels)
        if (_out[channel]!.isNotEmpty)
          channel: [
            for (final (i, entry) in _out[channel]!.cast<Json>().indexed)
              _asWritten(channel, entry, _wantedEntry(want, channel, entry, i)),
          ],
      if (fields.isNotEmpty) 'state': {for (final field in fields) field: _projectState(field)},
    };
    if (got['net'] case final List<Object?> net) got['net'] = _sortedBy(net, _netUrl);
    if (got['done'] case final List<Object?> done) got['done'] = _sortedBy(done, _doneId);
    _out = _noOutputs();
    return got;
  }

  /// The expected entry [entry] is compared with: a `done` entry the one of its call, the others by position.
  static Json? _wantedEntry(Json want, String channel, Json entry, int i) {
    final entries = ((want[channel] as List?) ?? const []).cast<Json>();
    if (channel == 'done') return entries.where((d) => d['id'] == entry['id']).firstOrNull;
    return entries.elementAtOrNull(i);
  }

  static Json _asWritten(String channel, Json entry, Json? want) {
    if (channel == 'send') {
      return want != null && want.containsKey('on') && !want.containsKey('type') ? entry : entry['msg'] as Json;
    }
    if (channel != 'done' || !entry.containsKey('error')) return entry;
    return {
      for (final MapEntry(:key, :value) in entry.entries)
        if ((key != 're' && key != 'message') || (want != null && want.containsKey(key))) key: value,
    };
  }

  /// A state field as the README's projection writes it.
  Object? _projectState(String field) {
    final state = party.state;
    return switch (field) {
      'connection' => state.connection.name,
      'peerId' => state.peerId,
      'room' => state.room?.rev,
      'media' => state.media?.title,
      'match' => switch (state.match) {
          null => null,
          final match => {'state': match.state.name, if (match.deltaMs != null) 'deltaMs': match.deltaMs},
        },
      'choice' => switch (state.choice) {
          null => null,
          final choice => {'id': choice.id, 'ref': choice.ref},
        },
      'mismatch' => switch (state.mismatch) {
          null => null,
          final mismatch => {'id': mismatch.candidate.id, 'deltaMs': mismatch.deltaMs},
        },
      'candidates' => [
          for (final ranked in state.candidates) {'id': ranked.candidate.id, 'deltaMs': ranked.deltaMs},
        ],
      'offsetMs' => state.offsetMs,
      'player' => state.player.name,
      'endReason' => state.endReason?.name,
      'isHost' => state.isHost,
      'canControl' => state.canControl,
      'roomPositionMs' => party.roomPositionMs(),
      _ => fail('unknown state field $field'),
    };
  }
}

Json _projectEvent(PartyEvent event) => switch (event) {
      ActionEvent() => {
          'type': 'action',
          'kind': event.kind,
          'byPeerId': event.byPeerId,
          'by': event.by?.peerId,
          'byMe': event.byMe,
          'room': event.room.rev,
        },
      ParticipantJoinedEvent() => {'type': 'participantJoined', 'peerId': event.participant.peerId},
      ParticipantLeftEvent() => {'type': 'participantLeft', 'peerId': event.participant.peerId},
      ErrorEvent() => {'type': 'error', 'code': event.error.code, 're': event.error.re},
      ResolveFailedEvent() => {'type': 'resolveFailed', 'timeout': event.error is TimeoutException},
      PlayBlockedEvent() => {'type': 'playBlocked'},
      EndedEvent() => {'type': 'ended', 'reason': event.reason.name, 'error': event.error?.code},
      _ => fail('unknown event $event'),
    };

double _round6(num rate) => (rate * 1e6).round() / 1e6;

/// The expected outputs of a step with every rate rounded as the driver records it, `net` per URL and `done` by id.
Json _expected(Json outputs) => {
      ...outputs,
      if (outputs['command'] case final List<Object?> commands)
        'command': [
          for (final command in commands.cast<Json>())
            command['do'] == 'setRate' ? {...command, 'rate': _round6(command['rate'] as num)} : command,
        ],
      if (outputs['net'] case final List<Object?> net) 'net': _sortedBy(net, _netUrl),
      if (outputs['done'] case final List<Object?> done) 'done': _sortedBy(done, _doneId),
    };

String _netUrl(Json e) => (e['get'] ?? e['open'] ?? e['close']) as String;

String _doneId(Json e) => e['id'] as String;

/// Sorts [entries] stably by [key]: `net` is pinned per URL only, so sorting by URL keeps each URL's order and drops
/// the rest; `done` isn't ordered at all, and each call reports once, so sorting by id drops its order.
List<Object?> _sortedBy(List<Object?> entries, String Function(Json e) key) {
  final indexed = entries.cast<Json>().indexed.toList()
    ..sort((a, b) => switch (key(a.$2).compareTo(key(b.$2))) { 0 => a.$1.compareTo(b.$1), final c => c });
  return [for (final (_, entry) in indexed) entry];
}
