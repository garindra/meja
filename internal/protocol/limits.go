package protocol

const (
	DefaultMaxFrameSize          = 4 << 20
	MaxRetainedPayloadCap        = 8 << 20
	MaxRetainedANSIOutput        = 8 << 20
	MaxStringLen          uint64 = 64 << 10
	MaxBytesLen           uint64 = 4 << 20
	MaxArgvCount          uint64 = 256
	MaxWindows            uint64 = 1024
	MaxVisiblePanes       uint64 = 4
	MaxRenderSlots        uint64 = 4
	MaxStyles             uint64 = 4096
	MaxGridCols           uint64 = 1024
	MaxGridRows           uint64 = 1024
	MaxCells              uint64 = MaxGridCols * MaxGridRows
	MaxCellRun            uint64 = MaxGridCols
)
