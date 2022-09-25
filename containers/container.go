package containers

import (
	"github.com/coroot/coroot-node-agent/cgroup"
	"github.com/coroot/coroot-node-agent/common"
	"github.com/coroot/coroot-node-agent/flags"
	"github.com/coroot/coroot-node-agent/logs"
	"github.com/coroot/coroot-node-agent/node"
	"github.com/coroot/coroot-node-agent/pinger"
	"github.com/coroot/coroot-node-agent/proc"
	"github.com/coroot/logparser"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netns"
	"inet.af/netaddr"
	"k8s.io/klog/v2"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	gcInterval  = 10 * time.Minute
	pingTimeout = 300 * time.Millisecond
)

type ContainerID string

type ContainerMetadata struct {
	name        string
	labels      map[string]string
	volumes     map[string]string
	logPath     string
	logDecoder  logparser.Decoder
	hostListens map[string][]netaddr.IPPort
}

type Delays struct {
	cpu  time.Duration
	disk time.Duration
}

type LogParser struct {
	parser *logparser.Parser
	stop   func()
}

func (p *LogParser) Stop() {
	if p.stop != nil {
		p.stop()
	}
	p.parser.Stop()
}

type AddrPair struct {
	src netaddr.IPPort
	dst netaddr.IPPort
}

type Container struct {
	cgroup   *cgroup.Cgroup
	metadata *ContainerMetadata

	pids map[uint32]time.Time // pid -> start time

	startedAt time.Time
	zombieAt  time.Time
	restarts  int

	delays      Delays
	delaysByPid map[uint32]Delays
	delaysLock  sync.Mutex

	listens map[netaddr.IPPort]map[uint32]time.Time // listen addr -> pid -> close time

	connectsSuccessful map[AddrPair]int             // dst:actual_dst -> count
	connectsFailed     map[netaddr.IPPort]int       // dst -> count
	connectLastAttempt map[netaddr.IPPort]time.Time // dst -> time
	connectionsActive  map[AddrPair]netaddr.IPPort  // src:dst -> actual_dst
	retransmits        map[AddrPair]int             // dst:actual_dst -> count

	oomKills int

	mountIds map[string]struct{}

	logParsers map[string]*LogParser

	lock sync.RWMutex

	done chan struct{}
}

func NewContainer(cg *cgroup.Cgroup, md *ContainerMetadata) *Container {
	c := &Container{
		cgroup:   cg,
		metadata: md,

		pids: map[uint32]time.Time{},

		delaysByPid: map[uint32]Delays{},

		listens: map[netaddr.IPPort]map[uint32]time.Time{},

		connectsSuccessful: map[AddrPair]int{},
		connectsFailed:     map[netaddr.IPPort]int{},
		connectLastAttempt: map[netaddr.IPPort]time.Time{},
		connectionsActive:  map[AddrPair]netaddr.IPPort{},
		retransmits:        map[AddrPair]int{},

		mountIds: map[string]struct{}{},

		logParsers: map[string]*LogParser{},

		done: make(chan struct{}),
	}

	c.runLogParser("")

	go func() {
		ticker := time.NewTicker(gcInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.done:
				return
			case t := <-ticker.C:
				c.gc(t)
			}
		}
	}()

	return c
}

func (c *Container) Close() {
	for _, p := range c.logParsers {
		p.Stop()
	}
	close(c.done)
}

func (c *Container) Dead(now time.Time) bool {
	return !c.zombieAt.IsZero() && now.Sub(c.zombieAt) > gcInterval
}

func (c *Container) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range metricsList {
		ch <- m
	}
}

func (c *Container) Collect(ch chan<- prometheus.Metric) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	ch <- counter(metrics.Restarts, float64(c.restarts))

	if cpu, err := c.cgroup.CpuStat(); err == nil {
		if cpu.LimitCores > 0 {
			ch <- gauge(metrics.CPULimit, cpu.LimitCores)
		}
		ch <- counter(metrics.CPUUsage, cpu.UsageSeconds)
		ch <- counter(metrics.ThrottledTime, cpu.ThrottledTimeSeconds)
	}

	if taskstatsClient != nil {
		c.updateDelays()
		ch <- counter(metrics.CPUDelay, float64(c.delays.cpu)/float64(time.Second))
		ch <- counter(metrics.DiskDelay, float64(c.delays.disk)/float64(time.Second))
	}

	if s, err := c.cgroup.MemoryStat(); err == nil {
		ch <- gauge(metrics.MemoryRss, float64(s.RSS))
		ch <- gauge(metrics.MemoryCache, float64(s.Cache))
		if s.Limit > 0 {
			ch <- gauge(metrics.MemoryLimit, float64(s.Limit))
		}
	}

	if c.oomKills > 0 {
		ch <- counter(metrics.OOMKills, float64(c.oomKills))
	}

	if disks, err := node.GetDisks(); err == nil {
		ioStat, _ := c.cgroup.IOStat()
		for majorMinor, mounts := range c.getMounts() {
			dev := disks.GetParentBlockDevice(majorMinor)
			if dev == nil {
				continue
			}
			for mountPoint, fsStat := range mounts {
				dls := []string{mountPoint, dev.Name, c.metadata.volumes[mountPoint]}
				ch <- gauge(metrics.DiskSize, float64(fsStat.CapacityBytes), dls...)
				ch <- gauge(metrics.DiskUsed, float64(fsStat.UsedBytes), dls...)
				ch <- gauge(metrics.DiskReserved, float64(fsStat.ReservedBytes), dls...)
				if io, ok := ioStat[majorMinor]; ok {
					ch <- counter(metrics.DiskReadOps, float64(io.ReadOps), dls...)
					ch <- counter(metrics.DiskReadBytes, float64(io.ReadBytes), dls...)
					ch <- counter(metrics.DiskWriteOps, float64(io.WriteOps), dls...)
					ch <- counter(metrics.DiskWriteBytes, float64(io.WrittenBytes), dls...)
				}
			}
		}
	}

	netNs := netns.None()
	for pid := range c.pids {
		if pid == agentPid {
			netNs = selfNetNs
			break
		}
		ns, err := proc.GetNetNs(pid)
		if err != nil {
			if !common.IsNotExist(err) {
				klog.Warningln(err)
			}
			continue
		}
		netNs = ns
		defer netNs.Close()
		break
	}

	listens := c.getListens(netNs)
	for addr, open := range listens {
		ch <- gauge(metrics.NetListenInfo, float64(open), addr.String(), "")
	}
	for proxy, addrs := range c.getProxiedListens() {
		for addr := range addrs {
			ch <- gauge(metrics.NetListenInfo, 1, addr.String(), proxy)
		}
	}

	for d, count := range c.connectsSuccessful {
		ch <- counter(metrics.NetConnectsSuccessful, float64(count), d.src.String(), d.dst.String())
	}
	for dst, count := range c.connectsFailed {
		ch <- counter(metrics.NetConnectsFailed, float64(count), dst.String())
	}
	for d, count := range c.retransmits {
		ch <- counter(metrics.NetRetransmits, float64(count), d.src.String(), d.dst.String())
	}

	connections := map[AddrPair]int{}
	for c, actualDst := range c.connectionsActive {
		connections[AddrPair{src: c.dst, dst: actualDst}]++
	}
	for d, count := range connections {
		ch <- gauge(metrics.NetConnectionsActive, float64(count), d.src.String(), d.dst.String())
	}

	for source, p := range c.logParsers {
		for _, c := range p.parser.GetCounters() {
			ch <- counter(metrics.LogMessages, float64(c.Messages), source, c.Level.String(), c.Hash, c.Sample)
		}
	}

	appTypes := map[string]struct{}{}
	for pid := range c.pids {
		cmdline := proc.GetCmdline(pid)
		if len(cmdline) == 0 {
			continue
		}
		appType := guessApplicationType(cmdline)
		if appType == "" {
			continue
		}
		appTypes[appType] = struct{}{}
	}
	for appType := range appTypes {
		ch <- gauge(metrics.ApplicationType, 1, appType)
	}

	if !*flags.NoPingUpstreams {
		for ip, rtt := range c.ping(netNs) {
			ch <- gauge(metrics.NetLatency, rtt, ip.String())
		}
	}
}

func (c *Container) onProcessStart(pid uint32) {
	c.lock.Lock()
	defer c.lock.Unlock()
	stats, err := TaskstatsPID(pid)
	if err != nil {
		return
	}
	c.zombieAt = time.Time{}
	c.pids[pid] = stats.BeginTime
	if c.startedAt.IsZero() {
		c.startedAt = stats.BeginTime
	} else {
		min := stats.BeginTime
		for _, t := range c.pids {
			if t.Before(min) {
				min = t
			}
		}
		if min.After(c.startedAt) {
			c.restarts++
			c.startedAt = min
		}
	}
}

func (c *Container) onProcessExit(pid uint32, oomKill bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.pids, pid)
	if len(c.pids) == 0 {
		c.zombieAt = time.Now()
	}
	delete(c.delaysByPid, pid)
	if oomKill {
		c.oomKills++
	}
}

func (c *Container) onFileOpen(pid uint32, fd uint32) {
	mntId, logPath := resolveFd(pid, fd)
	c.lock.Lock()
	defer c.lock.Unlock()
	if mntId != "" {
		c.mountIds[mntId] = struct{}{}
	}
	if logPath != "" {
		c.runLogParser(logPath)
	}
}

func (c *Container) onListenOpen(pid uint32, addr netaddr.IPPort, safe bool) {
	if !safe {
		c.lock.Lock()
		defer c.lock.Unlock()
	}
	if _, ok := c.listens[addr]; !ok {
		c.listens[addr] = map[uint32]time.Time{}
	}
	c.listens[addr][pid] = time.Time{}
}

func (c *Container) onListenClose(pid uint32, addr netaddr.IPPort) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if _, byAddr := c.listens[addr]; byAddr {
		if _, byPid := c.listens[addr][pid]; byPid {
			c.listens[addr][pid] = time.Now()
		}
	}
}

func (c *Container) onConnectionOpen(pid uint32, src, dst netaddr.IPPort, failed bool) {
	if dst.IP().IsLoopback() {
		netNs, err := proc.GetNetNs(pid)
		isHostNs := err == nil && hostNetNsId == netNs.UniqueId()
		netNs.Close()
		if !isHostNs {
			return
		}
	} else {
		whitelisted := false
		for _, prefix := range flags.ExternalNetworksWhitelist {
			if prefix.Contains(dst.IP()) {
				whitelisted = true
				break
			}
		}
		if !whitelisted && !common.IsIpPrivate(dst.IP()) {
			return
		}
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	if failed {
		c.connectsFailed[dst]++
	} else {
		actualDst := ConntrackGetActualDestination(src, dst)
		c.connectsSuccessful[AddrPair{src: dst, dst: actualDst}]++
		c.connectionsActive[AddrPair{src: src, dst: dst}] = actualDst
	}
	c.connectLastAttempt[dst] = time.Now()
}

func (c *Container) onConnectionClose(srcDst AddrPair) bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	if _, ok := c.connectionsActive[srcDst]; !ok {
		return false
	}
	delete(c.connectionsActive, srcDst)
	return true
}

func (c *Container) onRetransmit(srcDst AddrPair) bool {
	c.lock.Lock()
	defer c.lock.Unlock()
	actualDst, ok := c.connectionsActive[srcDst]
	if !ok {
		return false
	}
	c.retransmits[AddrPair{src: srcDst.dst, dst: actualDst}]++
	return true
}

func (c *Container) updateDelays() {
	c.delaysLock.Lock()
	defer c.delaysLock.Unlock()
	for pid := range c.pids {
		stats, err := TaskstatsTGID(pid)
		if err != nil {
			continue
		}
		d := c.delaysByPid[pid]
		c.delays.cpu += stats.CPUDelay - d.cpu
		c.delays.disk += stats.BlockIODelay - d.disk
		d.cpu = stats.CPUDelay
		d.disk = stats.BlockIODelay
		c.delaysByPid[pid] = d
	}
}

func (c *Container) getMounts() map[string]map[string]*proc.FSStat {
	mounts := map[string]proc.MountInfo{}
	for p := range c.pids {
		mi := proc.GetMountInfo(p)
		if mi != nil {
			mounts = mi
			break
		}
	}
	for mountId := range mounts {
		if _, ok := c.mountIds[mountId]; !ok {
			delete(mounts, mountId)
		}
	}
	if len(mounts) == 0 {
		return nil
	}
	res := map[string]map[string]*proc.FSStat{}
	for _, mi := range mounts {
		var stat *proc.FSStat
		for pid := range c.pids {
			s, err := proc.StatFS(proc.Path(pid, "root", mi.MountPoint))
			if err == nil {
				stat = &s
				break
			}
		}
		if stat == nil {
			continue
		}
		if _, ok := res[mi.MajorMinor]; !ok {
			res[mi.MajorMinor] = map[string]*proc.FSStat{}
		}
		res[mi.MajorMinor][mi.MountPoint] = stat
	}
	return res
}

func (c *Container) getListens(netNs netns.NsHandle) map[netaddr.IPPort]int {
	if !netNs.IsOpen() {
		return nil
	}
	isHostNs := hostNetNsId == netNs.UniqueId()
	res := map[netaddr.IPPort]int{}
	for addr, byPid := range c.listens {
		open := 0
		for _, closedAt := range byPid {
			if closedAt.IsZero() {
				open = 1
				break
			}
		}
		var ips []netaddr.IP
		if addr.IP().IsUnspecified() {
			if nsIps, err := proc.GetNsIps(netNs); err != nil {
				klog.Warningln(err)
			} else {
				ips = nsIps
			}
		} else {
			ips = []netaddr.IP{addr.IP()}
		}
		for _, ip := range ips {
			if ip.IsLoopback() && !isHostNs {
				continue
			}
			res[netaddr.IPPortFrom(ip, addr.Port())] = open
		}
	}
	return res
}

func (c *Container) getProxiedListens() map[string]map[netaddr.IPPort]struct{} {
	if len(c.metadata.hostListens) == 0 {
		return nil
	}

	hasUnspecified := false
	for _, addrs := range c.metadata.hostListens {
		for _, addr := range addrs {
			if addr.IP().IsUnspecified() {
				hasUnspecified = true
				break
			}
		}
	}

	var hostIps []netaddr.IP
	if hasUnspecified {
		if ns, err := proc.GetHostNetNs(); err != nil {
			klog.Warningln(err)
		} else {
			ips, err := proc.GetNsIps(ns)
			_ = ns.Close()
			if err != nil {
				klog.Warningln(err)
			} else {
				hostIps = ips
			}
		}
	}

	res := map[string]map[netaddr.IPPort]struct{}{}
	for proxy, addrs := range c.metadata.hostListens {
		res[proxy] = map[netaddr.IPPort]struct{}{}
		for _, addr := range addrs {
			if addr.IP().IsUnspecified() {
				for _, ip := range hostIps {
					if addr.IP().Is4() && ip.Is4() || addr.IP().Is6() && ip.Is6() {
						res[proxy][netaddr.IPPortFrom(ip, addr.Port())] = struct{}{}
					}
				}
			} else {
				res[proxy][addr] = struct{}{}
			}
		}
	}
	return res
}

func (c *Container) ping(netNs netns.NsHandle) map[netaddr.IP]float64 {
	if !netNs.IsOpen() {
		return nil
	}
	ips := map[netaddr.IP]struct{}{}
	for d := range c.connectsSuccessful {
		ips[d.dst.IP()] = struct{}{}
	}
	for dst := range c.connectsFailed {
		ips[dst.IP()] = struct{}{}
	}
	if len(ips) == 0 {
		return nil
	}
	targets := make([]netaddr.IP, 0, len(ips))
	for ip := range ips {
		targets = append(targets, ip)
	}
	rtt, err := pinger.Ping(netNs, selfNetNs, targets, pingTimeout)
	if err != nil {
		klog.Warningln(err)
		return nil
	}
	return rtt
}

func (c *Container) runLogParser(logPath string) {
	if *flags.NoParseLogs {
		return
	}

	if logPath != "" {
		if c.logParsers[logPath] != nil {
			return
		}
		ch := make(chan logparser.LogEntry)
		parser := logparser.NewParser(ch, nil)
		reader, err := logs.NewTailReader(proc.HostPath(logPath), ch)
		if err != nil {
			klog.Warningln(err)
			parser.Stop()
			return
		}
		klog.InfoS("started varlog logparser", "cg", c.cgroup.Id, "log", logPath)
		c.logParsers[logPath] = &LogParser{parser: parser, stop: reader.Stop}
		return
	}

	switch c.cgroup.ContainerType {
	case cgroup.ContainerTypeSystemdService:
		ch := make(chan logparser.LogEntry)
		if err := JournaldSubscribe(c.cgroup, ch); err != nil {
			klog.Warningln(err)
			return
		}
		parser := logparser.NewParser(ch, nil)
		stop := func() {
			JournaldUnsubscribe(c.cgroup)
		}
		klog.InfoS("started journald logparser", "cg", c.cgroup.Id)
		c.logParsers["journald"] = &LogParser{parser: parser, stop: stop}

	case cgroup.ContainerTypeDocker, cgroup.ContainerTypeContainerd:
		if c.metadata.logPath == "" {
			return
		}
		if parser := c.logParsers["stdout/stderr"]; parser != nil {
			parser.Stop()
			delete(c.logParsers, "stdout/stderr")
		}
		ch := make(chan logparser.LogEntry)
		parser := logparser.NewParser(ch, c.metadata.logDecoder)
		reader, err := logs.NewTailReader(proc.HostPath(c.metadata.logPath), ch)
		if err != nil {
			klog.Warningln(err)
			parser.Stop()
			return
		}
		klog.InfoS("started container logparser", "cg", c.cgroup.Id)
		c.logParsers["stdout/stderr"] = &LogParser{parser: parser, stop: reader.Stop}
	}
}

func (c *Container) gc(now time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()

	established := map[AddrPair]struct{}{}
	establishedDst := map[netaddr.IPPort]struct{}{}
	listens := map[netaddr.IPPort]string{}
	for pid := range c.pids {
		sockets, err := proc.GetSockets(pid)
		if err != nil {
			continue
		}
		for _, s := range sockets {
			if s.Listen {
				listens[s.SAddr] = s.Inode
			} else {
				established[AddrPair{src: s.SAddr, dst: s.DAddr}] = struct{}{}
				establishedDst[s.DAddr] = struct{}{}
			}
		}
		break
	}

	c.revalidateListens(now, listens)

	for srcDst := range c.connectionsActive {
		if _, ok := established[srcDst]; !ok {
			delete(c.connectionsActive, srcDst)
		}
	}
	for dst, at := range c.connectLastAttempt {
		_, active := establishedDst[dst]
		if !active && !at.IsZero() && now.Sub(at) > gcInterval {
			delete(c.connectLastAttempt, dst)
			delete(c.connectsFailed, dst)
			for d := range c.connectsSuccessful {
				if d.src == dst {
					delete(c.connectsSuccessful, d)
				}
			}
			for d := range c.retransmits {
				if d.src == dst {
					delete(c.retransmits, d)
				}
			}
		}
	}
}

func (c *Container) revalidateListens(now time.Time, actualListens map[netaddr.IPPort]string) {
	for addr, byPid := range c.listens {
		if _, open := actualListens[addr]; open {
			continue
		}
		klog.Warningln("deleting the outdated listen:", addr)
		for pid, closedAt := range byPid {
			if closedAt.IsZero() {
				byPid[pid] = now
			}
		}
	}

	missingListens := map[netaddr.IPPort]string{}
	for addr, inode := range actualListens {
		byPids, found := c.listens[addr]
		if !found {
			missingListens[addr] = inode
			continue
		}
		open := false
		for _, closedAt := range byPids {
			if closedAt.IsZero() {
				open = true
				break
			}
		}
		if !open {
			missingListens[addr] = inode
		}
	}

	if len(missingListens) > 0 {
		inodeToPid := map[string]uint32{}
		for pid := range c.pids {
			fds, err := proc.ReadFds(pid)
			if err != nil {
				continue
			}
			for _, fd := range fds {
				if fd.SocketInode != "" {
					inodeToPid[fd.SocketInode] = pid
				}
			}
		}
		for addr, inode := range missingListens {
			pid, found := inodeToPid[inode]
			if !found {
				continue
			}
			klog.Warningln("missing listen found:", addr, pid)
			c.onListenOpen(pid, addr, true)
		}
	}

	for addr, pids := range c.listens {
		for pid, closedAt := range pids {
			if !closedAt.IsZero() && now.Sub(closedAt) > gcInterval {
				delete(c.listens[addr], pid)
			}
		}
		if len(c.listens[addr]) == 0 {
			delete(c.listens, addr)
		}
	}
}

func resolveFd(pid uint32, fd uint32) (mntId string, logPath string) {
	info := proc.GetFdInfo(pid, fd)
	if info == nil {
		return
	}
	switch {
	case info.Flags&os.O_WRONLY == 0 && info.Flags&os.O_RDWR == 0,
		!strings.HasPrefix(info.Dest, "/"),
		strings.HasPrefix(info.Dest, "/proc/"),
		strings.HasPrefix(info.Dest, "/dev/"),
		strings.HasPrefix(info.Dest, "/sys/"),
		strings.HasSuffix(info.Dest, "(deleted)"):
		return
	}
	mntId = info.MntId

	if info.Flags&os.O_WRONLY != 0 && strings.HasPrefix(info.Dest, "/var/log/") &&
		!strings.HasPrefix(info.Dest, "/var/log/pods/") &&
		!strings.HasPrefix(info.Dest, "/var/log/containers/") &&
		!strings.HasPrefix(info.Dest, "/var/log/journal/") {

		logPath = info.Dest
	}
	return
}

func counter(desc *prometheus.Desc, value float64, labelValues ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(desc, prometheus.CounterValue, value, labelValues...)
}

func gauge(desc *prometheus.Desc, value float64, labelValues ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labelValues...)
}
