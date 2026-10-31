// Package netpipe is the Mac side of rubi-netpipe: the multiplexer that carries every TCP connection of a VM
// that has no network over one byte stream, the stdio of a long-lived `container exec -i ... rubi-netpipe`.
// The VM side (runner/container/rubi-netpipe, node) listens on 127.0.0.1:3128 (the CONNECT proxy) and
// 127.0.0.1:3129 (the auth gateway) and opens one stream per connection; this side hands each stream to the
// proxy or the gateway as a net.Conn, so nothing on the Mac listens for the VM at all.
//
// Wire format (docs/design/agent-runner.md, "No network: rubi-netpipe"). A frame is a 9-byte header, then the payload:
//
//	type   1 byte
//	stream 4 bytes, big endian (0 only for HELLO)
//	length 4 bytes, big endian, at most MaxPayload
//
// Types: HELLO (VM to Home, once, when its listeners are up; payload: 1 byte, Version), OPEN (payload: 1 byte,
// the service: SvcProxy or SvcGateway; sent by the side that accepted a connection), DATA, CLOSE (the sender
// writes no more: a half close), RESET (the stream is dead both ways) and ACK (payload: 4 bytes, how many bytes
// of DATA the reader has handed on). Flow control: each direction of each stream starts with Window bytes of
// credit, DATA spends it and ACK gives it back, so a slow reader stops its sender and no more than Window
// bytes are ever buffered per stream and direction. A frame that doesn't parse (unknown type, oversize,
// wrong length) is a protocol error that ends the whole channel; a frame for an unknown stream is ignored (the
// stream may just have been closed).
package netpipe
