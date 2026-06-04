package main

import (
	"fmt"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTrimLogLines(t *testing.T) {
	t.Parallel()

	got := trimLogLines([]string{"1", "2", "3", "4"}, 3)
	want := []string{"2", "3", "4"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trimLogLines returned %v, want %v", got, want)
	}
}

func TestBoundedLogWriterKeepsLatestLines(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	logPath := path.Join(tempDir, "cpu_memory_stress.log")

	var initialLines []string
	for i := 1; i <= 105; i++ {
		initialLines = append(initialLines, fmt.Sprintf("line-%d", i))
	}

	if err := os.WriteFile(logPath, []byte(strings.Join(initialLines, "\n")+"\n"), 0644); err != nil {
		t.Fatalf("write initial log file: %v", err)
	}

	writer, err := newBoundedLogWriter(logPath, 100)
	if err != nil {
		t.Fatalf("newBoundedLogWriter returned error: %v", err)
	}

	if err := writer.AppendLine("line-106"); err != nil {
		t.Fatalf("AppendLine returned error: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bounded log file: %v", err)
	}

	lines := splitLogLines(string(data))
	if len(lines) != 100 {
		t.Fatalf("bounded log file line count = %d, want 100", len(lines))
	}
	if lines[0] != initialLines[6] {
		t.Fatalf("first bounded line = %q, want %q", lines[0], initialLines[6])
	}
	if lines[len(lines)-1] != "line-106" {
		t.Fatalf("last bounded line = %q, want %q", lines[len(lines)-1], "line-106")
	}
}

func TestParseCPUTimes(t *testing.T) {
	t.Parallel()

	stat := "cpu  100 20 30 400 50 6 7 8 0 0\ncpu0 50 10 15 200 25 3 4 5 0 0\n"

	got, err := parseCPUTimes(stat)
	if err != nil {
		t.Fatalf("parseCPUTimes returned error: %v", err)
	}

	if got.total != 621 {
		t.Fatalf("parseCPUTimes total = %d, want 621", got.total)
	}
	if got.idle != 450 {
		t.Fatalf("parseCPUTimes idle = %d, want 450", got.idle)
	}
}

func TestCalculateCPUUsage(t *testing.T) {
	t.Parallel()

	previous := cpuTimes{idle: 100, total: 200}
	current := cpuTimes{idle: 120, total: 250}

	got, err := calculateCPUUsage(previous, current)
	if err != nil {
		t.Fatalf("calculateCPUUsage returned error: %v", err)
	}

	if got != 60 {
		t.Fatalf("calculateCPUUsage = %.2f, want 60.00", got)
	}
}

func TestCalculateMemoryUsagePercent(t *testing.T) {
	t.Parallel()

	got, err := calculateMemoryUsagePercent(memoryUsage{
		total:     1000,
		available: 250,
		used:      750,
	})
	if err != nil {
		t.Fatalf("calculateMemoryUsagePercent returned error: %v", err)
	}

	if got != 75 {
		t.Fatalf("calculateMemoryUsagePercent = %.2f, want 75.00", got)
	}
}

func TestPlanTotalLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		totalPercent int
		totalCPUs    int
		want         []int
	}{
		{
			name:         "single cpu",
			totalPercent: 65,
			totalCPUs:    1,
			want:         []int{65},
		},
		{
			name:         "partial worker remainder",
			totalPercent: 65,
			totalCPUs:    8,
			want:         []int{100, 100, 100, 100, 100, 20},
		},
		{
			name:         "small total target",
			totalPercent: 1,
			totalCPUs:    8,
			want:         []int{8},
		},
		{
			name:         "full load",
			totalPercent: 100,
			totalCPUs:    4,
			want:         []int{100, 100, 100, 100},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := planTotalLoad(tt.totalPercent, tt.totalCPUs)
			if err != nil {
				t.Fatalf("planTotalLoad returned error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("planTotalLoad(%d, %d) = %v, want %v", tt.totalPercent, tt.totalCPUs, got, tt.want)
			}
		})
	}
}

func TestPlanTotalLoadRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		totalPercent int
		totalCPUs    int
	}{
		{
			name:         "percent too low",
			totalPercent: 0,
			totalCPUs:    4,
		},
		{
			name:         "percent too high",
			totalPercent: 101,
			totalCPUs:    4,
		},
		{
			name:         "cpu count too low",
			totalPercent: 50,
			totalCPUs:    0,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := planTotalLoad(tt.totalPercent, tt.totalCPUs); err == nil {
				t.Fatalf("planTotalLoad(%d, %d) unexpectedly succeeded", tt.totalPercent, tt.totalCPUs)
			}
		})
	}
}

func TestCalculateCPUUsageRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		previous cpuTimes
		current  cpuTimes
	}{
		{
			name:     "total does not advance",
			previous: cpuTimes{idle: 100, total: 200},
			current:  cpuTimes{idle: 110, total: 200},
		},
		{
			name:     "idle moves backwards",
			previous: cpuTimes{idle: 100, total: 200},
			current:  cpuTimes{idle: 90, total: 250},
		},
		{
			name:     "idle advances more than total",
			previous: cpuTimes{idle: 100, total: 200},
			current:  cpuTimes{idle: 160, total: 250},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := calculateCPUUsage(tt.previous, tt.current); err == nil {
				t.Fatalf("calculateCPUUsage(%+v, %+v) unexpectedly succeeded", tt.previous, tt.current)
			}
		})
	}
}

func TestCalculateMemoryUsagePercentRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []memoryUsage{
		{total: 0, available: 0, used: 0},
		{total: 100, available: 101, used: 0},
	}

	for _, sample := range tests {
		sample := sample
		t.Run(fmt.Sprintf("%+v", sample), func(t *testing.T) {
			t.Parallel()

			if _, err := calculateMemoryUsagePercent(sample); err == nil {
				t.Fatalf("calculateMemoryUsagePercent(%+v) unexpectedly succeeded", sample)
			}
		})
	}
}

func TestParseMemTotalBytes(t *testing.T) {
	t.Parallel()

	meminfo := "MemTotal:       16384 kB\nMemAvailable:   12288 kB\nMemFree:         4096 kB\n"

	got, err := parseMemTotalBytes(meminfo)
	if err != nil {
		t.Fatalf("parseMemTotalBytes returned error: %v", err)
	}

	const want = 16384 * 1024
	if got != want {
		t.Fatalf("parseMemTotalBytes = %d, want %d", got, want)
	}
}

func TestParseMemAvailableBytes(t *testing.T) {
	t.Parallel()

	meminfo := "MemTotal:       16384 kB\nMemAvailable:   12288 kB\nMemFree:         4096 kB\n"

	got, err := parseMemAvailableBytes(meminfo)
	if err != nil {
		t.Fatalf("parseMemAvailableBytes returned error: %v", err)
	}

	const want = 12288 * 1024
	if got != want {
		t.Fatalf("parseMemAvailableBytes = %d, want %d", got, want)
	}
}

func TestReadMemoryUsage(t *testing.T) {
	t.Parallel()

	meminfo := "MemTotal:       16384 kB\nMemAvailable:   12288 kB\nMemFree:         4096 kB\n"

	total, available, err := func() (uint64, uint64, error) {
		total, err := parseMemTotalBytes(meminfo)
		if err != nil {
			return 0, 0, err
		}
		available, err := parseMemAvailableBytes(meminfo)
		if err != nil {
			return 0, 0, err
		}
		return total, available, nil
	}()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	sample := memoryUsage{
		total:     total,
		available: available,
		used:      total - available,
	}
	if sample.used != 4096*1024 {
		t.Fatalf("memoryUsage.used = %d, want %d", sample.used, 4096*1024)
	}
}

func TestParseMemTotalBytesRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"MemTotal:\n",
		"MemTotal: nope kB\n",
	}

	for _, input := range tests {
		input := input
		t.Run(strings.ReplaceAll(input, "\n", "\\n"), func(t *testing.T) {
			t.Parallel()

			if _, err := parseMemTotalBytes(input); err == nil {
				t.Fatalf("parseMemTotalBytes(%q) unexpectedly succeeded", input)
			}
		})
	}
}

func TestParseMemAvailableBytesRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"MemAvailable:\n",
		"MemAvailable: nope kB\n",
	}

	for _, input := range tests {
		input := input
		t.Run(strings.ReplaceAll(input, "\n", "\\n"), func(t *testing.T) {
			t.Parallel()

			if _, err := parseMemAvailableBytes(input); err == nil {
				t.Fatalf("parseMemAvailableBytes(%q) unexpectedly succeeded", input)
			}
		})
	}
}

func TestParseMemoryLimitBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  uint64
	}{
		{
			name:  "cgroup v2 unlimited",
			input: "max\n",
			want:  0,
		},
		{
			name:  "blank",
			input: " \n",
			want:  0,
		},
		{
			name:  "numeric",
			input: "1073741824\n",
			want:  1073741824,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseMemoryLimitBytes(tt.input)
			if err != nil {
				t.Fatalf("parseMemoryLimitBytes returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseMemoryLimitBytes(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseMemoryLimitBytesRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	if _, err := parseMemoryLimitBytes("not-a-number"); err == nil {
		t.Fatal("parseMemoryLimitBytes unexpectedly succeeded")
	}
}

func TestEffectiveMemoryAvailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		systemAvail uint64
		cgroupLimit uint64
		cgroupUse   uint64
		want        uint64
	}{
		{
			name:        "no cgroup limit",
			systemAvail: 8 << 30,
			cgroupLimit: 0,
			cgroupUse:   0,
			want:        8 << 30,
		},
		{
			name:        "smaller cgroup headroom wins",
			systemAvail: 8 << 30,
			cgroupLimit: 4 << 30,
			cgroupUse:   3 << 30,
			want:        1 << 30,
		},
		{
			name:        "system available lower than cgroup headroom",
			systemAvail: 2 << 30,
			cgroupLimit: 8 << 30,
			cgroupUse:   1 << 30,
			want:        2 << 30,
		},
		{
			name:        "cgroup exhausted",
			systemAvail: 2 << 30,
			cgroupLimit: 4 << 30,
			cgroupUse:   4 << 30,
			want:        0,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := effectiveMemoryAvailable(tt.systemAvail, tt.cgroupLimit, tt.cgroupUse)
			if err != nil {
				t.Fatalf("effectiveMemoryAvailable returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("effectiveMemoryAvailable(%d, %d, %d) = %d, want %d", tt.systemAvail, tt.cgroupLimit, tt.cgroupUse, got, tt.want)
			}
		})
	}
}

func TestEffectiveMemoryAvailableRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	if _, err := effectiveMemoryAvailable(0, 0, 0); err == nil {
		t.Fatal("effectiveMemoryAvailable unexpectedly succeeded")
	}
}

func TestApplySafetyMargin(t *testing.T) {
	t.Parallel()

	got, err := applySafetyMargin(1000, 10)
	if err != nil {
		t.Fatalf("applySafetyMargin returned error: %v", err)
	}
	if got != 900 {
		t.Fatalf("applySafetyMargin = %d, want 900", got)
	}
}

func TestApplySafetyMarginRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		available uint64
		safety    int
	}{
		{
			name:      "zero available",
			available: 0,
			safety:    10,
		},
		{
			name:      "negative safety",
			available: 100,
			safety:    -1,
		},
		{
			name:      "full safety",
			available: 100,
			safety:    100,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := applySafetyMargin(tt.available, tt.safety); err == nil {
				t.Fatalf("applySafetyMargin(%d, %d) unexpectedly succeeded", tt.available, tt.safety)
			}
		})
	}
}

func TestPlanMemoryLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		percent    int
		totalBytes uint64
		want       uint64
	}{
		{
			name:       "disabled",
			percent:    0,
			totalBytes: 8 << 30,
			want:       0,
		},
		{
			name:       "partial",
			percent:    25,
			totalBytes: 8 << 30,
			want:       2 << 30,
		},
		{
			name:       "rounds up tiny target",
			percent:    1,
			totalBytes: 99,
			want:       1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := planMemoryLoad(tt.percent, tt.totalBytes)
			if err != nil {
				t.Fatalf("planMemoryLoad returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("planMemoryLoad(%d, %d) = %d, want %d", tt.percent, tt.totalBytes, got, tt.want)
			}
		})
	}
}

func TestPlanMemoryLoadRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		percent    int
		totalBytes uint64
	}{
		{
			name:       "percent too low",
			percent:    -1,
			totalBytes: 8 << 30,
		},
		{
			name:       "percent too high",
			percent:    101,
			totalBytes: 8 << 30,
		},
		{
			name:       "zero total with non-zero target",
			percent:    10,
			totalBytes: 0,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := planMemoryLoad(tt.percent, tt.totalBytes); err == nil {
				t.Fatalf("planMemoryLoad(%d, %d) unexpectedly succeeded", tt.percent, tt.totalBytes)
			}
		})
	}
}

func TestWaitForDurationOrStopStopsEarly(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		waitForDurationOrStop(stop, time.Second)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	close(stop)

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waitForDurationOrStop did not stop early")
	}
}

func TestWaitForDurationOrStopWaitsForTimer(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	start := time.Now()
	waitForDurationOrStop(stop, 30*time.Millisecond)

	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("waitForDurationOrStop returned too early")
	}
}
