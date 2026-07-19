package protocol

const (
	// QUICInitialPacketSize is the RFC 9000 minimum. Keeping the UDP payload at
	// 1200 bytes leaves room for IP and UDP headers on 1280-byte paths instead
	// of relying on IP fragmentation during the handshake.
	QUICInitialPacketSize        = 1200
	DefaultMaxFrameSize          = 4 << 20
	MaxRetainedPayloadCap        = 8 << 20
	MaxRetainedANSIOutput        = 8 << 20
	MaxStringLen          uint64 = 64 << 10
	MaxBytesLen           uint64 = 4 << 20
	MaxArgvCount          uint64 = 256
	MaxVisiblePanes       uint64 = 8
	MaxRenderSlots        uint64 = 8
	OutputStreamCount     uint64 = MaxRenderSlots + 1
	StatusRenderSlot      uint8  = uint8(MaxRenderSlots)
	MaxGridCols           uint64 = 1024
	MaxGridRows           uint64 = 1024
	MaxGridCells                 = MaxGridCols * MaxGridRows
)

// OutputIndexFromStreamID maps server-initiated unidirectional QUIC stream IDs
// (3, 7, 11, ...) to Meja's connection-local display output indices. Index 0
// is the status surface; indices 1..8 correspond to pane slots 0..7.
func OutputIndexFromStreamID(id uint64) (uint8, bool) {
	if id&3 != 3 {
		return 0, false
	}
	index := (id - 3) / 4
	if index >= OutputStreamCount {
		return 0, false
	}
	return uint8(index), true
}
