package server

const (
	maxCSIParams        = 32
	maxCSIIntermediates = 2
)

type parserState int

const (
	parserText parserState = iota
	parserESC
	parserESCCharset
	parserCSI
	parserCSIDiscard
	parserOSC
	parserOSCESC
	parserDCS
	parserDCSESC
)

type Parser struct {
	state         parserState
	csiBuf        []byte
	utf8Buf       []byte
	charsetTarget int
}

func (p *Parser) clone() Parser {
	next := Parser{
		state:         p.state,
		csiBuf:        append([]byte(nil), p.csiBuf...),
		utf8Buf:       append([]byte(nil), p.utf8Buf...),
		charsetTarget: p.charsetTarget,
	}
	return next
}

type CSISequence struct {
	PrivatePrefix     byte
	Final             byte
	Params            [maxCSIParams]int
	ParamCount        int
	Intermediates     [maxCSIIntermediates]byte
	IntermediateCount int
}

func parseCSISequence(raw []byte, seq *CSISequence) bool {
	if len(raw) == 0 {
		return false
	}
	*seq = CSISequence{Final: raw[len(raw)-1]}
	body := raw[:len(raw)-1]
	if len(body) > 0 {
		switch body[0] {
		case '?', '>', '<', '=':
			seq.PrivatePrefix = body[0]
			body = body[1:]
		}
	}
	paramEnd := 0
	for paramEnd < len(body) {
		b := body[paramEnd]
		if (b >= '0' && b <= '9') || b == ';' {
			paramEnd++
			continue
		}
		break
	}
	if paramEnd > 0 {
		count := 0
		value := 0
		overflow := false
		for i := 0; i <= paramEnd; i++ {
			if i < paramEnd && body[i] != ';' {
				digit := int(body[i] - '0')
				if value > (int(^uint(0)>>1)-digit)/10 {
					overflow = true
				} else if !overflow {
					value = value*10 + digit
				}
				continue
			}
			if count == len(seq.Params) {
				return false
			}
			if overflow {
				value = 0
			}
			seq.Params[count] = value
			count++
			value, overflow = 0, false
		}
		seq.ParamCount = count
	}
	if paramEnd < len(body) {
		if len(body)-paramEnd > len(seq.Intermediates) {
			return false
		}
		seq.IntermediateCount = copy(seq.Intermediates[:], body[paramEnd:])
	}
	return true
}
