package main

import (
	"io"
	"os"
	"strings"

	"github.com/codemowers/slopify/player/internal/cleantext"
)

func hardwareDescription() string {
	for _, path := range []string{"/tmp/sysinfo/model", "/sys/firmware/devicetree/base/model", "/sys/devices/virtual/dmi/id/product_name"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, 4097))
		f.Close()
		if err != nil || len(data) > 4096 {
			continue
		}
		model := cleantext.Line(string(data), 120)
		for _, suffix := range []string{" (NOR/NAND)", " (NOR)", " (NAND)"} {
			model = strings.TrimSuffix(model, suffix)
		}
		if model != "" {
			return model
		}
	}
	return ""
}
