package runner

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func availableMemory() (uint64, error) {
	if runtime.GOOS == "linux" {
		file, err := os.Open("/proc/meminfo")
		if err != nil {
			return 0, err
		}
		defer file.Close()
		scan := bufio.NewScanner(file)
		for scan.Scan() {
			fields := strings.Fields(scan.Text())
			if len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
				value, err := strconv.ParseUint(fields[1], 10, 64)
				return value * 1024, err
			}
		}
		if err = scan.Err(); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("MemAvailable missing")
	}
	if runtime.GOOS == "darwin" {
		data, err := exec.Command("/usr/bin/vm_stat").Output()
		if err != nil {
			return 0, err
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) == 0 {
			return 0, fmt.Errorf("empty vm_stat")
		}
		header := strings.Fields(lines[0])
		var pageSize uint64
		for i, f := range header {
			if f == "of" && i+1 < len(header) {
				pageSize, _ = strconv.ParseUint(header[i+1], 10, 64)
			}
		}
		if pageSize == 0 {
			return 0, fmt.Errorf("vm_stat page size missing")
		}
		var pages uint64
		found := 0
		for _, line := range lines[1:] {
			key, value, ok := strings.Cut(line, ":")
			if ok && (key == "Pages free" || key == "Pages inactive" || key == "Pages speculative") {
				n, e := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
				if e != nil {
					return 0, e
				}
				pages += n
				found++
			}
		}
		if found != 3 {
			return 0, fmt.Errorf("vm_stat available counters missing")
		}
		return pages * pageSize, nil
	}
	return 0, fmt.Errorf("unsupported memory measurement platform")
}
