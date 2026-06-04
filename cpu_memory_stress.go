package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var sink uint64
var retainedMemory [][]byte

const (
	cpuLogInterval             = time.Second
	maxLogLines                = 100
	memoryChunkSize            = 64 << 20
	defaultMemorySafetyPercent = 10
)

type cpuTimes struct {
	idle  uint64
	total uint64
}

type memoryUsage struct {
	total     uint64
	available uint64
	used      uint64
}

type boundedLogWriter struct {
	path     string
	maxLines int
	mu       sync.Mutex
	lines    []string
}

func burnCPU(stop <-chan struct{}, busy time.Duration) {
	start := time.Now()
	var local uint64

	for {
		select {
		case <-stop:
			atomic.AddUint64(&sink, local)
			return
		default:
		}

		if time.Since(start) >= busy {
			atomic.AddUint64(&sink, local)
			return
		}

		for i := 0; i < 10000; i++ {
			local = local*1664525 + 1013904223 + uint64(i)
		}
	}
}

func worker(stop <-chan struct{}, workerID int, percent int, slice time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()

	busy := time.Duration(int64(slice) * int64(percent) / 100)
	idle := slice - busy

	for {
		select {
		case <-stop:
			return
		default:
		}

		burnCPU(stop, busy)

		if idle > 0 {
			timer := time.NewTimer(idle)
			select {
			case <-stop:
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
		}
	}
}

func detectLogicalCPUs() int {
	cpus := runtime.NumCPU()
	if cpus < 1 {
		return 1
	}
	return cpus
}

func parseMemInfoBytes(meminfo string, fieldName string) (uint64, error) {
	for _, line := range strings.Split(meminfo, "\n") {
		if !strings.HasPrefix(line, fieldName+":") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("invalid %s line: %q", fieldName, line)
		}

		valueKB, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse %s value %q: %w", fieldName, fields[1], err)
		}

		return valueKB * 1024, nil
	}

	return 0, fmt.Errorf("%s line not found in /proc/meminfo", fieldName)
}

func parseMemTotalBytes(meminfo string) (uint64, error) {
	return parseMemInfoBytes(meminfo, "MemTotal")
}

func parseMemAvailableBytes(meminfo string) (uint64, error) {
	return parseMemInfoBytes(meminfo, "MemAvailable")
}

func readSystemMemoryInfo() (uint64, uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}

	totalBytes, err := parseMemTotalBytes(string(data))
	if err != nil {
		return 0, 0, err
	}

	availableBytes, err := parseMemAvailableBytes(string(data))
	if err != nil {
		return 0, 0, err
	}

	return totalBytes, availableBytes, nil
}

func readMemoryUsage() (memoryUsage, error) {
	totalBytes, availableBytes, err := readSystemMemoryInfo()
	if err != nil {
		return memoryUsage{}, err
	}
	if availableBytes > totalBytes {
		return memoryUsage{}, fmt.Errorf("invalid memory info: available %d exceeds total %d", availableBytes, totalBytes)
	}

	return memoryUsage{
		total:     totalBytes,
		available: availableBytes,
		used:      totalBytes - availableBytes,
	}, nil
}

func parseMemoryLimitBytes(raw string) (uint64, error) {
	value := strings.TrimSpace(raw)
	if value == "" || value == "max" {
		return 0, nil
	}

	limit, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse memory limit %q: %w", value, err)
	}

	return limit, nil
}

func readCgroupMemoryLimit() (uint64, error) {
	paths := []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	}

	for _, cgroupPath := range paths {
		data, err := os.ReadFile(cgroupPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", cgroupPath, err)
		}

		limit, err := parseMemoryLimitBytes(string(data))
		if err != nil {
			return 0, fmt.Errorf("parse %s: %w", cgroupPath, err)
		}

		return limit, nil
	}

	return 0, nil
}

func readCgroupMemoryCurrent() (uint64, error) {
	paths := []string{
		"/sys/fs/cgroup/memory.current",
		"/sys/fs/cgroup/memory/memory.usage_in_bytes",
	}

	for _, cgroupPath := range paths {
		data, err := os.ReadFile(cgroupPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", cgroupPath, err)
		}

		current, err := parseMemoryLimitBytes(string(data))
		if err != nil {
			return 0, fmt.Errorf("parse %s: %w", cgroupPath, err)
		}

		return current, nil
	}

	return 0, nil
}

func clampUint64(value uint64, minValue uint64, maxValue uint64) uint64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}

	return value
}

func applySafetyMargin(availableBytes uint64, safetyPercent int) (uint64, error) {
	if safetyPercent < 0 || safetyPercent >= 100 {
		return 0, fmt.Errorf("invalid safety percent: %d", safetyPercent)
	}
	if availableBytes == 0 {
		return 0, fmt.Errorf("invalid available memory: %d", availableBytes)
	}

	headroom := availableBytes * uint64(safetyPercent) / 100
	if headroom >= availableBytes {
		return 0, fmt.Errorf("memory safety margin leaves no allocatable memory")
	}

	return availableBytes - headroom, nil
}

func effectiveMemoryAvailable(systemAvailable uint64, cgroupLimit uint64, cgroupCurrent uint64) (uint64, error) {
	if systemAvailable == 0 {
		return 0, fmt.Errorf("invalid system available memory: %d", systemAvailable)
	}
	if cgroupLimit == 0 {
		return systemAvailable, nil
	}
	if cgroupCurrent >= cgroupLimit {
		return 0, nil
	}

	cgroupAvailable := cgroupLimit - cgroupCurrent
	return clampUint64(cgroupAvailable, 0, systemAvailable), nil
}

func detectMemoryAvailable() (uint64, error) {
	_, systemAvailable, err := readSystemMemoryInfo()
	if err != nil {
		return 0, err
	}

	cgroupLimit, err := readCgroupMemoryLimit()
	if err != nil {
		return 0, err
	}

	cgroupCurrent, err := readCgroupMemoryCurrent()
	if err != nil {
		return 0, err
	}

	return effectiveMemoryAvailable(systemAvailable, cgroupLimit, cgroupCurrent)
}

func releaseReservedMemory() {
	retainedMemory = nil
	runtime.GC()
}

func waitForDurationOrStop(stop <-chan struct{}, duration time.Duration) {
	if duration <= 0 {
		<-stop
		return
	}

	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-stop:
	}
}

func planMemoryLoad(percent int, allocatableBytes uint64) (uint64, error) {
	if percent < 0 || percent > 100 {
		return 0, fmt.Errorf("invalid memory percent: %d", percent)
	}
	if percent == 0 {
		return 0, nil
	}
	if allocatableBytes == 0 {
		return 0, fmt.Errorf("invalid allocatable memory: %d", allocatableBytes)
	}

	targetBytes := allocatableBytes * uint64(percent) / 100
	if targetBytes == 0 {
		targetBytes = 1
	}

	return targetBytes, nil
}

func touchMemoryPages(chunk []byte, offset uint64, pageSize int) {
	if len(chunk) == 0 {
		return
	}

	for i := 0; i < len(chunk); i += pageSize {
		pageIndex := offset/uint64(pageSize) + uint64(i/pageSize)
		chunk[i] = byte(pageIndex%251 + 1)
	}

	chunk[len(chunk)-1] = byte((offset+uint64(len(chunk)-1))%251 + 1)
}

func reserveMemory(targetBytes uint64) ([][]byte, error) {
	if targetBytes == 0 {
		return nil, nil
	}

	pageSize := os.Getpagesize()
	if pageSize < 1 {
		pageSize = 4096
	}

	chunks := make([][]byte, 0)
	remaining := targetBytes
	var offset uint64

	for remaining > 0 {
		chunkBytes := uint64(memoryChunkSize)
		if chunkBytes > remaining {
			chunkBytes = remaining
		}

		chunk := make([]byte, int(chunkBytes))
		touchMemoryPages(chunk, offset, pageSize)
		chunks = append(chunks, chunk)

		remaining -= chunkBytes
		offset += chunkBytes
	}

	return chunks, nil
}

func formatBytes(bytes uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(bytes)
	unit := 0

	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}

	if unit == 0 {
		return fmt.Sprintf("%d%s", bytes, units[unit])
	}

	return fmt.Sprintf("%.2f%s", value, units[unit])
}

func newBoundedLogWriter(logPath string, maxLines int) (*boundedLogWriter, error) {
	if maxLines < 1 {
		return nil, fmt.Errorf("invalid max log lines: %d", maxLines)
	}

	writer := &boundedLogWriter{
		path:     logPath,
		maxLines: maxLines,
	}

	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read log file: %w", err)
	}
	if err == nil {
		writer.lines = trimLogLines(splitLogLines(string(data)), maxLines)
	}

	if err := writer.flushLocked(); err != nil {
		return nil, err
	}

	return writer, nil
}

func defaultLogPath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}

	exeName := path.Base(exePath)
	baseName := strings.TrimSuffix(exeName, path.Ext(exeName))
	if baseName == "" {
		baseName = "cpu_memory_stress"
	}

	return path.Join(path.Dir(exePath), baseName+".log"), nil
}

func splitLogLines(content string) []string {
	trimmed := strings.TrimRight(content, "\n")
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "\n")
}

func trimLogLines(lines []string, maxLines int) []string {
	if maxLines < 1 || len(lines) == 0 {
		return nil
	}
	if len(lines) <= maxLines {
		return append([]string(nil), lines...)
	}

	return append([]string(nil), lines[len(lines)-maxLines:]...)
}

func (w *boundedLogWriter) AppendLine(line string) error {
	line = strings.TrimRight(line, "\n")

	w.mu.Lock()
	defer w.mu.Unlock()

	w.lines = append(w.lines, line)
	w.lines = trimLogLines(w.lines, w.maxLines)

	return w.flushLocked()
}

func (w *boundedLogWriter) flushLocked() error {
	content := strings.Join(w.lines, "\n")
	if len(w.lines) > 0 {
		content += "\n"
	}

	tempPath := w.path + ".tmp"
	if err := os.WriteFile(tempPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write temp log file: %w", err)
	}
	if err := os.Rename(tempPath, w.path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace log file: %w", err)
	}

	return nil
}

func parseCPUTimes(stat string) (cpuTimes, error) {
	for _, line := range strings.Split(stat, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 5 {
			return cpuTimes{}, fmt.Errorf("invalid /proc/stat cpu line: %q", line)
		}

		var sample cpuTimes
		for idx, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("parse cpu field %q: %w", field, err)
			}

			sample.total += value
			if idx == 3 || idx == 4 {
				sample.idle += value
			}
		}

		return sample, nil
	}

	return cpuTimes{}, fmt.Errorf("cpu line not found in /proc/stat")
}

func readCPUTimes() (cpuTimes, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, fmt.Errorf("read /proc/stat: %w", err)
	}

	return parseCPUTimes(string(data))
}

func calculateCPUUsage(previous cpuTimes, current cpuTimes) (float64, error) {
	if current.total <= previous.total {
		return 0, fmt.Errorf("cpu total did not advance")
	}
	if current.idle < previous.idle {
		return 0, fmt.Errorf("cpu idle moved backwards")
	}

	totalDelta := current.total - previous.total
	idleDelta := current.idle - previous.idle
	if idleDelta > totalDelta {
		return 0, fmt.Errorf("cpu idle advanced more than total")
	}
	busyDelta := totalDelta - idleDelta

	return float64(busyDelta) * 100 / float64(totalDelta), nil
}

func calculateMemoryUsagePercent(sample memoryUsage) (float64, error) {
	if sample.total == 0 {
		return 0, fmt.Errorf("memory total is zero")
	}
	if sample.available > sample.total {
		return 0, fmt.Errorf("memory available exceeds total")
	}

	return float64(sample.used) * 100 / float64(sample.total), nil
}

func logSystemUsage(stop <-chan struct{}, logWriter *boundedLogWriter, wg *sync.WaitGroup) {
	defer wg.Done()

	previous, err := readCPUTimes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cpu logger init failed: %v\n", err)
		return
	}

	ticker := time.NewTicker(cpuLogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			current, err := readCPUTimes()
			if err != nil {
				fmt.Fprintf(os.Stderr, "cpu logger read failed: %v\n", err)
				continue
			}
			memory, err := readMemoryUsage()
			if err != nil {
				fmt.Fprintf(os.Stderr, "memory logger read failed: %v\n", err)
				continue
			}

			cpuUsage, err := calculateCPUUsage(previous, current)
			previous = current
			if err != nil {
				fmt.Fprintf(os.Stderr, "cpu logger sample failed: %v\n", err)
				continue
			}
			memoryUsagePercent, err := calculateMemoryUsagePercent(memory)
			if err != nil {
				fmt.Fprintf(os.Stderr, "memory logger sample failed: %v\n", err)
				continue
			}

			if err := logWriter.AppendLine(fmt.Sprintf(
				"%s total_cpu=%.2f%% total_mem=%.2f%% mem_used_bytes=%d mem_available_bytes=%d mem_total_bytes=%d",
				now.UTC().Format(time.RFC3339),
				cpuUsage,
				memoryUsagePercent,
				memory.used,
				memory.available,
				memory.total,
			)); err != nil {
				fmt.Fprintf(os.Stderr, "system logger write failed: %v\n", err)
			}
		}
	}
}

func planTotalLoad(totalPercent int, totalCPUs int) ([]int, error) {
	if totalPercent < 1 || totalPercent > 100 {
		return nil, fmt.Errorf("invalid total percent: %d", totalPercent)
	}
	if totalCPUs < 1 {
		return nil, fmt.Errorf("invalid cpu count: %d", totalCPUs)
	}

	totalBudget := totalPercent * totalCPUs
	fullWorkers := totalBudget / 100
	partialWorker := totalBudget % 100

	workerPercents := make([]int, 0, fullWorkers+1)
	for i := 0; i < fullWorkers; i++ {
		workerPercents = append(workerPercents, 100)
	}
	if partialWorker > 0 {
		workerPercents = append(workerPercents, partialWorker)
	}

	return workerPercents, nil
}

func formatWorkerTargets(workerPercents []int) string {
	if len(workerPercents) == 0 {
		return ""
	}

	parts := make([]string, 0, len(workerPercents))
	for _, percent := range workerPercents {
		parts = append(parts, fmt.Sprintf("%d%%", percent))
	}

	return strings.Join(parts, ",")
}

func formatDuration(seconds int) string {
	if seconds == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%ds", seconds)
}

func usage() {
	prog := os.Args[0]
	fmt.Fprintf(os.Stderr, "Usage: %s [-t threads] [-p percent] [-total percent] [-m percent] [-d seconds] [-s slice_ms]\n\n", prog)
	fmt.Fprintln(os.Stderr, "Options:")
	fmt.Fprintln(os.Stderr, "  -t threads   Number of worker goroutines. Default: online CPU cores")
	fmt.Fprintln(os.Stderr, "  -p percent   Target CPU load per worker (0-100). Default: 100")
	fmt.Fprintln(os.Stderr, "  -total pct   Target total CPU load across all logical CPU cores (1-100)")
	fmt.Fprintln(os.Stderr, "               Auto-detects core count and overrides -t/-p")
	fmt.Fprintln(os.Stderr, "  -m percent   Target memory reservation as a percent of allocatable memory (0-100)")
	fmt.Fprintf(os.Stderr, "               Uses MemAvailable and keeps %d%% safety headroom\n", defaultMemorySafetyPercent)
	fmt.Fprintln(os.Stderr, "               Alias: -mem; when used alone, CPU load stays disabled")
	fmt.Fprintln(os.Stderr, "  -d seconds   Run duration. Default: no limit. Use 0 for no limit")
	fmt.Fprintln(os.Stderr, "  -s slice_ms  Control interval in milliseconds. Default: 100")
	fmt.Fprintln(os.Stderr, "  -h           Show this help")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintf(os.Stderr, "  %s -t 4 -p 100 -d 120\n", prog)
	fmt.Fprintf(os.Stderr, "  %s -t 8 -p 70\n", prog)
	fmt.Fprintf(os.Stderr, "  %s -total 65\n", prog)
	fmt.Fprintf(os.Stderr, "  %s -m 30 -d 60\n", prog)
}

func main() {
	defaultThreads := detectLogicalCPUs()

	threads := flag.Int("t", defaultThreads, "")
	percent := flag.Int("p", 100, "")
	totalPercent := flag.Int("total", 0, "")
	memoryPercent := flag.Int("m", 0, "")
	flag.IntVar(memoryPercent, "mem", 0, "")
	duration := flag.Int("d", 0, "")
	sliceMS := flag.Int("s", 100, "")
	help := flag.Bool("h", false, "")

	flag.Usage = usage
	flag.Parse()

	if *help {
		usage()
		return
	}

	if *threads < 1 {
		fmt.Fprintf(os.Stderr, "invalid threads: %d\n", *threads)
		os.Exit(1)
	}
	if *percent < 0 || *percent > 100 {
		fmt.Fprintf(os.Stderr, "invalid percent: %d\n", *percent)
		os.Exit(1)
	}
	if *totalPercent < 0 || *totalPercent > 100 {
		fmt.Fprintf(os.Stderr, "invalid total percent: %d\n", *totalPercent)
		os.Exit(1)
	}
	if *memoryPercent < 0 || *memoryPercent > 100 {
		fmt.Fprintf(os.Stderr, "invalid memory percent: %d\n", *memoryPercent)
		os.Exit(1)
	}
	if *duration < 0 {
		fmt.Fprintf(os.Stderr, "invalid duration: %d\n", *duration)
		os.Exit(1)
	}
	if *sliceMS < 1 {
		fmt.Fprintf(os.Stderr, "invalid slice_ms: %d\n", *sliceMS)
		os.Exit(1)
	}

	detectedCPUs := detectLogicalCPUs()
	workerPercents := make([]int, 0, *threads)
	cpuMode := "per-worker"
	cpuFlagsExplicit := false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "t", "p", "total":
			cpuFlagsExplicit = true
		}
	})

	if *totalPercent > 0 {
		var err error
		workerPercents, err = planTotalLoad(*totalPercent, detectedCPUs)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cpuMode = "total"
	} else if *percent > 0 && !(*memoryPercent > 0 && !cpuFlagsExplicit) {
		for i := 0; i < *threads; i++ {
			workerPercents = append(workerPercents, *percent)
		}
	} else {
		cpuMode = "disabled"
	}

	memoryAvailableBytes := uint64(0)
	memoryAllocatableBytes := uint64(0)
	memoryTargetBytes := uint64(0)
	if *memoryPercent > 0 {
		var err error
		memoryAvailableBytes, err = detectMemoryAvailable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		memoryAllocatableBytes, err = applySafetyMargin(memoryAvailableBytes, defaultMemorySafetyPercent)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		memoryTargetBytes, err = planMemoryLoad(*memoryPercent, memoryAllocatableBytes)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	if len(workerPercents) == 0 && memoryTargetBytes == 0 {
		fmt.Fprintln(os.Stderr, "no stress target specified")
		os.Exit(1)
	}

	gomaxprocs := len(workerPercents)
	if gomaxprocs < 1 {
		gomaxprocs = 1
	}
	runtime.GOMAXPROCS(gomaxprocs)

	logPath, err := defaultLogPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve log path failed: %v\n", err)
		os.Exit(1)
	}

	logWriter, err := newBoundedLogWriter(logPath, maxLogLines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize log file failed: %v\n", err)
		os.Exit(1)
	}

	if memoryTargetBytes > 0 {
		retainedMemory, err = reserveMemory(memoryTargetBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reserve memory failed: %v\n", err)
			os.Exit(1)
		}
	}

	if err := logWriter.AppendLine(fmt.Sprintf(
		"%s started cpu_mode=%s logical_cpus=%d workers=%d duration=%s slice=%dms log_interval=%s",
		time.Now().UTC().Format(time.RFC3339),
		cpuMode,
		detectedCPUs,
		len(workerPercents),
		formatDuration(*duration),
		*sliceMS,
		cpuLogInterval,
	)); err != nil {
		fmt.Fprintf(os.Stderr, "write start log failed: %v\n", err)
		os.Exit(1)
	}
	if *totalPercent > 0 {
		if err := logWriter.AppendLine(fmt.Sprintf("%s total_target=%d%% worker_targets=[%s]", time.Now().UTC().Format(time.RFC3339), *totalPercent, formatWorkerTargets(workerPercents))); err != nil {
			fmt.Fprintf(os.Stderr, "write target log failed: %v\n", err)
			os.Exit(1)
		}
	} else if len(workerPercents) > 0 {
		if err := logWriter.AppendLine(fmt.Sprintf("%s per_worker_target=%d%%", time.Now().UTC().Format(time.RFC3339), *percent)); err != nil {
			fmt.Fprintf(os.Stderr, "write target log failed: %v\n", err)
			os.Exit(1)
		}
	}
	if memoryTargetBytes > 0 {
		if err := logWriter.AppendLine(fmt.Sprintf(
			"%s memory_target=%d%% available_bytes=%d allocatable_bytes=%d reserved_bytes=%d safety_margin_percent=%d",
			time.Now().UTC().Format(time.RFC3339),
			*memoryPercent,
			memoryAvailableBytes,
			memoryAllocatableBytes,
			memoryTargetBytes,
			defaultMemorySafetyPercent,
		)); err != nil {
			fmt.Fprintf(os.Stderr, "write memory log failed: %v\n", err)
			os.Exit(1)
		}
	}

	stop := make(chan struct{})
	var once sync.Once
	stopAll := func() {
		once.Do(func() {
			close(stop)
		})
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	go func() {
		<-signals
		stopAll()
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go logSystemUsage(stop, logWriter, &wg)

	slice := time.Duration(*sliceMS) * time.Millisecond
	for i, workerPercent := range workerPercents {
		wg.Add(1)
		go worker(stop, i, workerPercent, slice, &wg)
	}

	if *duration > 0 {
		waitForDurationOrStop(stop, time.Duration(*duration)*time.Second)
		stopAll()
	} else {
		<-stop
	}

	releaseReservedMemory()
	wg.Wait()
	if err := logWriter.AppendLine(fmt.Sprintf("%s stopped sink=%d", time.Now().UTC().Format(time.RFC3339), atomic.LoadUint64(&sink))); err != nil {
		fmt.Fprintf(os.Stderr, "write stop log failed: %v\n", err)
	}
}
