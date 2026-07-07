// Package hksv implements HomeKit Secure Video (HKSV) for the Protect bridge.
//
// HKSV does not reuse the RTP/SRTP live-streaming path. Recordings travel over
// the HomeKit Data Stream (HDS) — a separate, encrypted TCP transport — as
// fragmented MP4. brutella/hap provides none of this, so the whole stack is
// built here on top of the library's generic service/characteristic primitives.
//
// Pieces:
//
//   - opack.go       Apple's OPACK binary serialization (HDS message payloads).
//   - tlv.go         HomeKit TLV8 codec (fragmentation + list delimiters).
//   - config.go      Builders/parsers for the HKSV configuration characteristics.
//   - service.go     The three HKSV services (CameraRecordingManagement,
//     CameraOperatingMode, DataStreamTransportManagement) and
//     their characteristics.
//   - hds.go         HDS frame codec: HKDF-SHA512 key derivation from the HAP
//     Pair-Verify shared secret, ChaCha20-Poly1305 framing, and
//     the header/message payload split.
//   - datastream.go  The HDS protocol: control "hello" handshake and the
//     "dataSend" recording flow, plus the per-session TCP server.
//   - recording.go   The ffmpeg prebuffer pipeline and fMP4 fragment production.
//   - mp4.go         Splits the fMP4 byte stream into the init segment and
//     media fragments.
//   - manager.go     Per-camera glue: reacts to the controller writing the
//     recording configuration / toggling active / setting up a
//     data stream, and drives the pipeline.
//
// The shared secret needed for HDS key derivation is not exposed by the stock
// hap API; the vendored copy in third_party/hap adds
// hap.SharedKeyForRequest for exactly this.
//
// Testing: the codecs (OPACK, TLV8, HDS framing, config TLVs, fMP4 splitting)
// and the dataSend protocol flow are covered by unit tests. End-to-end
// behaviour — a real controller negotiating keys, opening the stream and
// accepting the fragments — can only be verified against a physical Home hub
// recording to iCloud, since HKSV does not activate without one.
package hksv
