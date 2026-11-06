// Package netpipe is the Mac side of rubi-netpipe: the multiplexer that carries every TCP connection of a VM
// that has no network over one byte stream, the stdio of a long-lived `container exec -i ... rubi-netpipe`.
// The VM side (runner/container/rubi-netpipe, node) listens on 127.0.0.1:3128 (the CONNECT proxy), 127.0.0.1:3130 (the
// approval hook gate) and, for a T3 session, on the port of T3's own server, and opens one stream per connection; this side
// hands each stream to the proxy, the hook gate or the relay to T3 as a net.Conn, so nothing on the Mac listens for the VM at
// all. Service 2 (SvcGateway) was the auth gateway; it is reserved and refused.
//
// Wire format (docs/design/agent-runner.md, "No network: rubi-netpipe"). A frame is a 9-byte header, then the payload:
//
//	type   1 byte
//	stream 4 bytes, big endian (0 only for HELLO)
//	length 4 bytes, big endian, at most MaxPayload
//
// Types: HELLO (VM to Home, once, when its listeners are up; payload: 1 byte, Version), OPEN (payload: 1 byte,
// the service: SvcProxy, SvcHook, SvcT3 or SvcTestAPI; sent by the side that accepted a connection), DATA, CLOSE (the sender
// writes no more: a half close; closing a stream sends one), RESET (the stream is dead both ways: a refused
// stream, a violation, a closed stream that gets data, a peer that never answers a close) and ACK (payload: 4 bytes, how many bytes
// of DATA the reader has handed on). Flow control: each direction of each stream starts with Window bytes of
// credit, DATA spends it and ACK gives it back, so a slow reader stops its sender and no more than Window
// bytes are ever buffered per stream and direction. A frame that doesn't parse (unknown type, oversize,
// wrong length) is a protocol error that ends the whole channel; a frame for an unknown stream is ignored (the
// stream may just have been closed).
package netpipe
