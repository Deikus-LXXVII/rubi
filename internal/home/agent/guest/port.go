package guest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FindPort is the device of the virtio-serial port named name (ControlPortName), found the way udev would without udev
// (the guest has none): each /sys/class/virtio-ports/vportNpM has a "name" file, and the device is /dev/vportNpM. sys and
// dev are the roots ("/sys", "/dev"; a test's folders).
func FindPort(sys, dev, name string) (string, error) {
	dirs, err := filepath.Glob(filepath.Join(sys, "class", "virtio-ports", "vport*"))
	if err != nil {
		return "", err
	}
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "name"))
		if err != nil || strings.TrimSpace(string(b)) != name {
			continue
		}
		return filepath.Join(dev, filepath.Base(d)), nil
	}
	return "", fmt.Errorf("guest: no virtio-serial port named %s", name)
}

// ControlPortName is the port QEMU gives the control channel (microvm.ControlPort).
const ControlPortName = "rubi.control"
