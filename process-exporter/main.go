package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"regexp"
	"strings"
	"time"

	// "github.com/ncabatoff/gopsutil/process" // use my fork until shirou/gopsutil issue#235 fixed, but needs branch fix-internal-pkgref
	"github.com/prometheus/client_golang/prometheus"
	"github.com/shirou/gopsutil/process"
)

func printManual() {
	fmt.Print(`process-exporter process selection

By default every process is lumped into the "other" bucket, such that its
actions are accounted for in metrics with the label groupname="other".
The following options override that behaviour to allow you to group proceses
that should be tracked with distinct metrics.

namemapping allows assigning a group name based on a combination of the process
name and command line.  For example, using 

  -namemapping "python2,([^/]+\.py),java,-jar\s+([^/+]).jar)" 

will make it so that each different python2 and java -jar invocation will be
tracked with distinct metrics, *IF* they aren't in the "other" bucket.  The
remaining options below govern what is treated as "other" and what is not.

procnames is a comma-seperated list of process names that should get their own
metrics.  Even if no such processes are running, metrics will be created with a
groupname for each of the specific process names.

minIoPercent and minCpuPercent look at the total IO and CPU observed during a
collection cycle.  If a process that are currently in the "other" bucket 
has IO or CPU from that cycle which exceeds the threshold, it gets moved out
of the "other" bucket.
`)
}

var (
	numprocsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_num_procs",
		"number of processes in this group",
		[]string{"groupname"},
		nil)

	cpusecsDesc = prometheus.NewDesc(
		"namedprocess_namegroup_cpu_seconds_total",
		"cpu usage in seconds",
		[]string{"groupname"},
		nil)

	readbytesDesc = prometheus.NewDesc(
		"namedprocess_namegroup_read_bytes_total",
		"number of bytes read by this group",
		[]string{"groupname"},
		nil)

	writebytesDesc = prometheus.NewDesc(
		"namedprocess_namegroup_write_bytes_total",
		"number of bytes written by this group",
		[]string{"groupname"},
		nil)

	membytesDesc = prometheus.NewDesc(
		"namedprocess_namegroup_memory_bytes",
		"number of bytes of memory in use",
		[]string{"groupname", "memtype"},
		nil)
)

type (
	dummyResponseWriter struct {
		bytes.Buffer
		header http.Header
	}

	nameMapper struct {
		mapping map[string]*regexp.Regexp
	}
)

func (nm nameMapper) get(name, cmdline string) string {
	if regex, ok := nm.mapping[name]; ok {
		matches := regex.FindStringSubmatch(cmdline)
		if len(matches) > 1 {
			for _, matchstr := range matches[1:] {
				if matchstr != "" {
					return name + ":" + matchstr
				}
			}
		}
	}
	return name
}

func (d *dummyResponseWriter) Header() http.Header {
	return d.header
}

func (d *dummyResponseWriter) WriteHeader(code int) {
}

func parseNameMapper(s string) (*nameMapper, error) {
	mapper := make(map[string]*regexp.Regexp)
	if s == "" {
		return &nameMapper{mapper}, nil
	}

	toks := strings.Split(s, ",")
	if len(toks)%2 == 1 {
		return nil, fmt.Errorf("bad namemapper: odd number of tokens")
	}

	for i, tok := range toks {
		if tok == "" {
			return nil, fmt.Errorf("bad namemapper: token %d is empty", i)
		}
		if i%2 == 1 {
			name, regexstr := toks[i-1], tok
			if r, err := regexp.Compile(regexstr); err != nil {
				return nil, fmt.Errorf("error compiling regexp '%s': %v", regexstr, err)
			} else {
				mapper[name] = r
			}
		}
	}

	return &nameMapper{mapper}, nil
}

func main() {
	var (
		listenAddress = flag.String("web.listen-address", ":9256",
			"Address on which to expose metrics and web interface.")
		metricsPath = flag.String("web.telemetry-path", "/metrics",
			"Path under which to expose metrics.")
		onceToStdout = flag.Bool("once-to-stdout", false,
			"Don't bind, instead just print the metrics once to stdout and exit")
		procNames = flag.String("procnames", "",
			"comma-seperated list of process names to monitor")
		minIoPercent = flag.Float64("min-io-pct", 10.0,
			"percent of total I/O seen needed to promote out of 'other'")
		minCpuPercent = flag.Float64("min-cpu-pct", 10.0,
			"percent of total CPU seen needed to promote out of 'other'")
		nameMapping = flag.String("namemapping", "",
			"comma-seperated list, alternating process name and capturing regex to apply to cmdline")
		man = flag.Bool("man", false,
			"print manual")
	)
	flag.Parse()

	if *man {
		printManual()
		return
	}

	var names []string
	for _, s := range strings.Split(*procNames, ",") {
		if s != "" {
			names = append(names, s)
		}
	}

	namemapper, err := parseNameMapper(*nameMapping)
	if err != nil {
		log.Fatalf("Error parsing -namemapping argument '%s': %v", *nameMapping, err)
	}

	pc := NewProcessCollector(*minIoPercent, *minCpuPercent, names, namemapper)

	if err := pc.Init(); err != nil {
		log.Fatalf("Error initializing: %v", err)
	}

	prometheus.MustRegister(pc)

	if *onceToStdout {
		drw := dummyResponseWriter{header: make(http.Header)}
		httpreq, err := http.NewRequest("GET", "/metrics", nil)
		if err != nil {
			log.Fatalf("Error building request: %v", err)
		}

		prometheus.Handler().ServeHTTP(&drw, httpreq)
		drw.Buffer.Truncate(0)

		// We throw away the first result because that first collection primes the pump, and
		// otherwise we won't see our counter metrics.  This is specific to the implementation
		// of NamedProcessCollector.Collect().
		prometheus.Handler().ServeHTTP(&drw, httpreq)
		fmt.Print(drw.String())
		return
	}

	http.Handle(*metricsPath, prometheus.Handler())

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>Sensor Exporter</title></head>
			<body>
			<h1>Named Process Exporter</h1>
			<p><a href="` + *metricsPath + `">Metrics</a></p>
			</body>
			</html>`))
	})
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		log.Fatalf("Unable to setup HTTP server: %v", err)
	}
}

type (
	NamedProcessCollector struct {
		minIoPercent  float64
		minCpuPercent float64
		wantProcNames map[string]struct{}
		*nameMapper
		// track how much was seen last time so we can report the delta
		groupStats map[string]counts
		tracker    *procTracker
	}

	counts struct {
		cpu        float64
		readbytes  uint64
		writebytes uint64
	}

	groupcounts struct {
		counts
		procs       int
		memresident uint64
		memvirtual  uint64
		memswap     uint64
	}

	// procSum contains data read from /proc/pid/*
	procSum struct {
		name        string
		cmdline     string
		cpu         float64
		readbytes   uint64
		writebytes  uint64
		startTime   time.Time
		memresident uint64
		memvirtual  uint64
		memswap     uint64
	}

	trackedProc struct {
		lastUpdate time.Time
		lastvals   procSum
		accum      counts
	}

	processId struct {
		pid       int32
		startTime time.Time
	}

	procTracker struct {
		procs map[processId]trackedProc
		accum counts
	}
)

func (ps procSum) String() string {
	cmd := ps.cmdline
	if len(cmd) > 20 {
		cmd = cmd[:20]
	}
	return fmt.Sprintf("%20s %20s %7.0f %12d %12d", ps.name, cmd, ps.cpu, ps.readbytes, ps.writebytes)
}

func NewProcessCollector(minIoPercent float64, minCpuPercent float64, procnames []string, nameMapper *nameMapper) *NamedProcessCollector {
	pc := NamedProcessCollector{
		minIoPercent:  minIoPercent,
		minCpuPercent: minCpuPercent,
		wantProcNames: make(map[string]struct{}),
		nameMapper:    nameMapper,
		groupStats:    make(map[string]counts),
		tracker:       NewProcTracker(),
	}
	for _, name := range procnames {
		pc.wantProcNames[name] = struct{}{}
	}
	return &pc
}

func (p *NamedProcessCollector) Init() error {
	_, err := p.tracker.update()
	return err
}

// Describe implements prometheus.Collector.
func (p *NamedProcessCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- cpusecsDesc
	ch <- numprocsDesc
	ch <- readbytesDesc
	ch <- writebytesDesc
	ch <- membytesDesc
}

// Collect implements prometheus.Collector.
func (p *NamedProcessCollector) Collect(ch chan<- prometheus.Metric) {
	for gname, gcounts := range p.getGroups() {
		ch <- prometheus.MustNewConstMetric(numprocsDesc,
			prometheus.GaugeValue, float64(gcounts.procs), gname)
		ch <- prometheus.MustNewConstMetric(membytesDesc,
			prometheus.GaugeValue, float64(gcounts.memresident), gname, "resident")
		ch <- prometheus.MustNewConstMetric(membytesDesc,
			prometheus.GaugeValue, float64(gcounts.memvirtual), gname, "virtual")
		ch <- prometheus.MustNewConstMetric(membytesDesc,
			prometheus.GaugeValue, float64(gcounts.memswap), gname, "swap")

		if grpstat, ok := p.groupStats[gname]; ok {
			// It's convenient to treat cpu, readbytes, etc as counters so we can use rate().
			// In practice it doesn't quite work because processes can exit while their group
			// persists, and with that pid's absence our summed value across the group will
			// become smaller.  We'll cheat by simply pretending there was no change to the
			// counter when that happens.

			dcpu := gcounts.cpu - grpstat.cpu
			if dcpu < 0 {
				dcpu = 0
			}
			ch <- prometheus.MustNewConstMetric(cpusecsDesc,
				prometheus.CounterValue,
				dcpu,
				gname)

			drbytes := gcounts.readbytes - grpstat.readbytes
			if drbytes < 0 {
				drbytes = 0
			}
			ch <- prometheus.MustNewConstMetric(readbytesDesc,
				prometheus.CounterValue,
				float64(drbytes),
				gname)

			dwbytes := gcounts.writebytes - grpstat.writebytes
			if dwbytes < 0 {
				dwbytes = 0
			}
			ch <- prometheus.MustNewConstMetric(writebytesDesc,
				prometheus.CounterValue,
				float64(dwbytes),
				gname)
		}

		p.groupStats[gname] = gcounts.counts
	}
}

func (p *NamedProcessCollector) getGroups() map[string]groupcounts {
	delta, err := p.tracker.update()
	if err != nil {
		log.Fatalf("Error reading procs: %v", err)
	}

	totdeltaio := float64(delta.readbytes + delta.writebytes)
	totdeltacpu := float64(delta.cpu + delta.cpu)
	gcounts := make(map[string]groupcounts)

	for _, pinfo := range p.tracker.procs {
		gname := p.nameMapper.get(pinfo.lastvals.name, pinfo.lastvals.cmdline)
		if _, ok := p.wantProcNames[gname]; !ok {
			deltaio := float64(pinfo.accum.readbytes + pinfo.accum.writebytes)
			iopct := 100 * deltaio / totdeltaio
			deltacpu := float64(pinfo.accum.readbytes + pinfo.accum.writebytes)
			cpupct := 100 * deltacpu / totdeltacpu
			if iopct >= p.minIoPercent || cpupct >= p.minCpuPercent/totdeltacpu {
				p.wantProcNames[gname] = struct{}{}
			} else {
				gname = "other"
			}
		}

		cur := gcounts[gname]
		cur.procs++
		cur.memresident += pinfo.lastvals.memresident
		cur.memvirtual += pinfo.lastvals.memvirtual
		cur.memswap += pinfo.lastvals.memswap
		cur.counts.cpu += pinfo.accum.cpu
		cur.counts.readbytes += pinfo.accum.readbytes
		cur.counts.writebytes += pinfo.accum.writebytes
		gcounts[gname] = cur
	}

	return gcounts
}

func NewProcTracker() *procTracker {
	return &procTracker{make(map[processId]trackedProc), counts{}}
}

// Scan /proc and update oneself.  Rather than allocating a new map each time to detect procs
// that have disappeared, we bump the last update time on those that are still present.  Then
// as a second pass we traverse the map looking for stale procs and removing them.

func (t procTracker) update() (delta counts, err error) {
	pids, err := process.Pids()
	if err != nil {
		return delta, fmt.Errorf("Error reading procs: %v", err)
	}

	now := time.Now()
	for _, pid := range pids {
		psum, err := getProcSummary(pid)
		if err != nil {
			continue
		}
		procid := processId{pid, psum.startTime}

		var newaccum counts
		if cur, ok := t.procs[procid]; ok {
			newaccum = cur.accum

			dcpu := psum.cpu - cur.lastvals.cpu
			newaccum.cpu += dcpu
			delta.cpu += dcpu

			drbytes := psum.readbytes - cur.lastvals.readbytes
			newaccum.readbytes += drbytes
			delta.readbytes += drbytes

			dwbytes := psum.writebytes - cur.lastvals.writebytes
			newaccum.writebytes += dwbytes
			delta.writebytes += dwbytes
			// log.Printf("%9d %20s %.1f %6d %6d", procid.pid, psum.name, dcpu, drbytes, dwbytes)
		}
		t.procs[procid] = trackedProc{lastUpdate: now, lastvals: psum, accum: newaccum}
	}

	for procid, pinfo := range t.procs {
		if pinfo.lastUpdate != now {
			delete(t.procs, procid)
		}
	}

	return delta, nil
}

var (
	ErrUnnamed   = errors.New("unnamed proc")
	ErrNoCommand = errors.New("proc has empty cmdline")
)

func getProcSummary(pid int32) (procSum, error) {
	proc, err := process.NewProcess(pid)
	var psum procSum
	if err != nil {
		// errors happens so routinely (e.g. when we race) that it's not worth reporting IMO
		return psum, err
	}

	times, err := proc.Times()
	if err != nil {
		return psum, err
	}

	name, err := proc.Name()
	if err != nil {
		return psum, err
	}

	if name == "" {
		// these all appear to be kernel processes, which people generally don't care about
		// monitoring directly (i.e. the system OS metrics suffice)
		return psum, ErrUnnamed
	}

	cmdline, err := proc.Cmdline()
	if err != nil {
		return psum, err
	}

	if cmdline == "" {
		// these all appear to be kernel processes, which people generally don't care about
		// monitoring directly (i.e. the system OS metrics suffice)
		return psum, ErrNoCommand
	}

	ios, err := proc.IOCounters()
	if err != nil {
		return psum, err
	}

	ctime, err := proc.CreateTime()
	if err != nil {
		return psum, err
	}

	meminfo, err := proc.MemoryInfo()
	if err != nil {
		return psum, err
	}

	psum.name = name
	psum.cmdline = cmdline
	psum.cpu = times.User + times.System
	psum.writebytes = ios.WriteBytes
	psum.readbytes = ios.ReadBytes
	psum.startTime = time.Unix(ctime, 0)
	psum.memresident = meminfo.RSS
	psum.memvirtual = meminfo.VMS
	psum.memswap = meminfo.Swap

	return psum, nil
}