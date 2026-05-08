//go:build linux

package finops

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// RAPLSensor maintains a permanent File Descriptor to sysfs avoiding os.File heap escapes
type RAPLSensor struct {
	fd          int
	lastMicroJ  uint64
	initialized bool
}

func MountRAPL() (*RAPLSensor, error) {
	path := "/sys/class/powercap/intel-rapl/intel-rapl:0/energy_uj"
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("[SRE-VETO] Imposible acoplar driver Intel RAPL: %v", err)
	}

	sensor := &RAPLSensor{fd: fd}
	// Initial read
	uj, _ := sensor.readRawUJ()
	sensor.lastMicroJ = uj
	sensor.initialized = true

	return sensor, nil
}

// ReadWatts_O1 executes a PREAD Syscall (0 allocs) and converts MicroJoules Delta to Watts
func (r *RAPLSensor) ReadWatts_O1(deltaTimeSeconds float64) float64 {
	currentUj, ok := r.readRawUJ()
	if !ok || deltaTimeSeconds <= 0 {
		return 0
	}

	// [SRE-BUG-FIX] Handle RAPL counter wrap-around (resets every ~60min on Intel)
	var deltaUj uint64
	if currentUj >= r.lastMicroJ {
		deltaUj = currentUj - r.lastMicroJ
	} else {
		// Counter wrapped — skip this sample, use 0 delta
		deltaUj = 0
	}
	r.lastMicroJ = currentUj

	// 1 Joule = 1,000,000 MicroJoules. Watts = Joules / Seconds
	joules := float64(deltaUj) / 1000000.0
	return joules / deltaTimeSeconds
}

// readRawUJ executes the pure Unix Pread dumping directly to stack memory
func (r *RAPLSensor) readRawUJ() (uint64, bool) {
	var buf [32]byte
	n, err := unix.Pread(r.fd, buf[:], 0)
	if err != nil || n == 0 {
		return 0, false
	}

	var val uint64
	for i := range n {
		b := buf[i]
		if b >= '0' && b <= '9' {
			val = val*10 + uint64(b-'0')
		} else if b == '\n' {
			break
		}
	}
	return val, true
}

func (r *RAPLSensor) Close() {
	unix.Close(r.fd)
}
