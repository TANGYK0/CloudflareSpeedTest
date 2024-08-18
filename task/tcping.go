package task

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/XIU2/CloudflareSpeedTest/utils"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const (
	tcpConnectTimeout = time.Second * 1
	maxRoutine        = 1000
	defaultRoutines   = 200
	defaultPort       = 443
	defaultPingTimes  = 4
)

var (
	Icmping   bool
	Routines      = defaultRoutines
	TCPPort   int = defaultPort
	PingTimes int = defaultPingTimes
)

type Ping struct {
	wg      *sync.WaitGroup
	m       *sync.Mutex
	ips     []*net.IPAddr
	csv     utils.PingDelaySet
	control chan bool
	bar     *utils.Bar
}

func checkPingDefault() {
	if Routines <= 0 {
		Routines = defaultRoutines
	}
	if TCPPort <= 0 || TCPPort >= 65535 {
		TCPPort = defaultPort
	}
	if PingTimes <= 0 {
		PingTimes = defaultPingTimes
	}
}

func NewPing() *Ping {
	checkPingDefault()
	ips := loadIPRanges()
	return &Ping{
		wg:      &sync.WaitGroup{},
		m:       &sync.Mutex{},
		ips:     ips,
		csv:     make(utils.PingDelaySet, 0),
		control: make(chan bool, Routines),
		bar:     utils.NewBar(len(ips), "可用:", ""),
	}
}

func (p *Ping) Run() utils.PingDelaySet {
	if len(p.ips) == 0 {
		return p.csv
	}
	if Httping {
		fmt.Printf("开始延迟测速（模式：HTTP, 端口：%d, 范围：%v ~ %v ms, 丢包：%.2f)\n", TCPPort, utils.InputMinDelay.Milliseconds(), utils.InputMaxDelay.Milliseconds(), utils.InputMaxLossRate)
	} else if Icmping {
		fmt.Printf("开始延迟测速（模式：ICMP, 范围：%v ~ %v ms, 丢包：%.2f)\n", utils.InputMinDelay.Milliseconds(), utils.InputMaxDelay.Milliseconds(), utils.InputMaxLossRate)
	} else {
		fmt.Printf("开始延迟测速（模式：TCP, 端口：%d, 范围：%v ~ %v ms, 丢包：%.2f)\n", TCPPort, utils.InputMinDelay.Milliseconds(), utils.InputMaxDelay.Milliseconds(), utils.InputMaxLossRate)
	}
	for _, ip := range p.ips {
		p.wg.Add(1)
		p.control <- false
		go p.start(ip)
	}
	p.wg.Wait()
	p.bar.Done()
	sort.Sort(p.csv)
	return p.csv
}

func (p *Ping) start(ip *net.IPAddr) {
	defer p.wg.Done()
	p.tcpingHandler(ip)
	<-p.control
}

// bool connectionSucceed float32 time
func (p *Ping) tcping(ip *net.IPAddr) (bool, time.Duration) {
	startTime := time.Now()
	var fullAddress string
	if isIPv4(ip.String()) {
		fullAddress = fmt.Sprintf("%s:%d", ip.String(), TCPPort)
	} else {
		fullAddress = fmt.Sprintf("[%s]:%d", ip.String(), TCPPort)
	}
	conn, err := net.DialTimeout("tcp", fullAddress, tcpConnectTimeout)
	if err != nil {
		return false, 0
	}
	defer conn.Close()
	duration := time.Since(startTime)
	return true, duration
}

// pingReceived pingTotalTime
func (p *Ping) checkConnection(ip *net.IPAddr) (recv int, totalDelay time.Duration) {
	if Httping {
		recv, totalDelay = p.httping(ip)
		return
	}
	if Icmping {
		for i := 0; i < PingTimes; i++ {
			if ok, delay := p.icmping(ip); ok {
				recv++
				totalDelay += delay
			}
		}
		return
	}
	for i := 0; i < PingTimes; i++ {
		if ok, delay := p.tcping(ip); ok {
			recv++
			totalDelay += delay
		}
	}
	return
}

func (p *Ping) appendIPData(data *utils.PingData) {
	p.m.Lock()
	defer p.m.Unlock()
	p.csv = append(p.csv, utils.CloudflareIPData{
		PingData: data,
	})
}

// handle tcping
func (p *Ping) tcpingHandler(ip *net.IPAddr) {
	recv, totalDlay := p.checkConnection(ip)
	nowAble := len(p.csv)
	if recv != 0 {
		nowAble++
	}
	p.bar.Grow(1, strconv.Itoa(nowAble))
	if recv == 0 {
		return
	}
	data := &utils.PingData{
		IP:       ip,
		Sended:   PingTimes,
		Received: recv,
		Delay:    totalDlay / time.Duration(recv),
	}
	p.appendIPData(data)
}

func (p *Ping) icmping(ip *net.IPAddr) (bool, time.Duration) {
	// 创建一个原始套接字，用于发送 ICMP 数据包	var fullAddress string
	currentUser, _ := user.Current()

	if currentUser.Uid != "0" {
		fmt.Println("请使用sudo cf命令开启root权限")
		os.Exit(0)
	}
	conn, err := net.Dial("ip4:icmp", ip.String())
	if err != nil {
		return false, 0
	}
	defer conn.Close()

	// 创建 ICMP Echo 请求消息
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   os.Getpid() & 0xffff,
			Seq:  1,
			Data: []byte("Hello!"),
		},
	}

	// 编码消息
	msgBytes, err := msg.Marshal(nil)
	if err != nil {
		return false, 0
	}

	// 记录开始时间
	start := time.Now()

	// 发送 ICMP 请求
	_, err = conn.Write(msgBytes)
	if err != nil {
		return false, 0
	}

	// 接收 ICMP 响应
	buf := make([]byte, 1500)
	conn.SetReadDeadline(time.Now().Add(tcpConnectTimeout)) // 设置读取超时时间
	n, err := conn.Read(buf)
	if err != nil {
		return false, 0
	}
	// 记录结束时间
	duration := time.Since(start)

	// 解析响应消息
	response, err := icmp.ParseMessage(1, buf[20:n])
	if err != nil {
		return false, 0
	}

	// 检查响应类型
	if response.Type != ipv4.ICMPTypeEchoReply {
		return false, 0
	}

	return true, duration
}
