// Package e2e drives whole messages through kenward's real wiring.
//
// Every other package in this module is tested in isolation against fakes at its
// own boundary. That structurally cannot see a wiring mistake: a supervisor that
// hands a unit the wrong scope resolver, a group conversation that ends up with a
// member's private space in its Read set, a refusal path that quietly widens a
// tier chain. Those bugs live between packages, so the test that catches them has
// to span them.
//
// The tests here therefore build a real config.Config from a real YAML file, a
// real supervisor.Simple in simple mode, a real transport.Mux, real
// assistant.Units, a real capture.Engine, a real scope.Resolve, a real
// routing.Pool over the real HTTP completer, and a real session.Manager. Only the
// three outermost edges are faked, and each is faked at the place where kenward
// stops and someone else's process starts:
//
//   - lore, behind the memory.Memory interface, by a recorder that remembers
//     which spaces were asked for as well as what came back;
//   - the inference endpoints, by httptest servers speaking the
//     OpenAI-compatible wire format, so keel/llm and the pool's connect probe,
//     cooldown and tier walk all run for real;
//   - Telegram, by the transport.Fake the transport package already exports.
//
// Nothing here touches the network beyond loopback, a real lore, a real provider
// or a real bot, and everything it writes goes to t.TempDir.
package e2e
