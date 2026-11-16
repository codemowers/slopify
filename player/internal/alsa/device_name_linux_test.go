//go:build linux

package alsa

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUSBOutputNameUsesSelectedCardAncestry(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "selected-usb")
	iface := filepath.Join(device, "interface", "sound")
	if err := os.MkdirAll(iface, 0755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"idVendor": "1234", "idProduct": "5678", "manufacturer": "Maker\n", "product": "Model\x00\n"} {
		if err := os.WriteFile(filepath.Join(device, name), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if got := usbOutputName(iface); got != "Maker Model" {
		t.Fatalf("USB label %q", got)
	}
	if err := os.WriteFile(filepath.Join(device, "product"), []byte("Maker Model"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := usbOutputName(iface); got != "Maker Model" {
		t.Fatalf("repeated manufacturer: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(root, "non-usb"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := usbOutputName(filepath.Join(root, "non-usb")); got != "" {
		t.Fatalf("unrelated USB device chosen: %q", got)
	}
}
