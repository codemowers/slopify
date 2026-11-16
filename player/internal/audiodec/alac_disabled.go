//go:build no_alac

package audiodec

const alacEnabled = false

type ALAC struct{}

func NewALAC(ALACConfig) (*ALAC, error)           { return nil, disabledCodec("ALAC") }
func (*ALAC) Format() Format                      { return Format{Codec: "ALAC"} }
func (*ALAC) Decode([]byte, []int32) (int, error) { return 0, disabledCodec("ALAC") }
