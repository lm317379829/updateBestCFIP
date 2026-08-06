package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-ping/ping"
	log "github.com/sirupsen/logrus"
)

const (
	defaultLatency = 9999
	lossThreshold  = 35.0
	maxRetries     = 5
	pingWorkers    = 64
)

// 全局共享一份优选IP, 避免对每个域名重复下载和探测
var (
	BestIP string
	IPList []LatencyResult
)

type Config struct {
	Email       string     `json:"email"`
	Key         string     `json:"key"`
	DomainInfos [][]string `json:"domainInfos"`
}

type LatencyResult struct {
	Latency  int     `json:"latency"`
	IP       string  `json:"ip"`
	LossRate float64 `json:"lossRate"`
}

type zoneReply struct {
	Success bool `json:"success"`
	Results []struct {
		ID string `json:"id"`
	} `json:"result"`
}

type dnsReply struct {
	Success bool `json:"success"`
	Results []struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Proxied bool   `json:"proxied"`
	} `json:"result"`
}

type updateReply struct {
	Success bool `json:"success"`
}

func getLatency(target string, count int) LatencyResult {
	res := LatencyResult{Latency: defaultLatency, IP: target, LossRate: 1.00}

	pinger, err := ping.NewPinger(target)
	if err != nil {
		log.Info(fmt.Sprintf("ping 错误: %s", err))
		return res
	}
	pinger.SetPrivileged(true)
	pinger.Count = count
	pinger.Timeout = time.Second
	pinger.Run()

	stats := pinger.Statistics()
	if stats.PacketLoss < lossThreshold {
		res.Latency = int(stats.AvgRtt.Seconds() * 1000)
		res.LossRate = stats.PacketLoss
	}
	return res
}

func getResultList(content, blockArea string) []LatencyResult {
	rawLines := strings.Split(strings.Trim(content, "\n"), "\n")
	var targets []string

	for _, line := range rawLines {
		ip := strings.TrimSpace(line)
		if ip == "" {
			continue
		}
		if idx := strings.Index(ip, "#"); idx >= 0 {
			if blockArea != "" && strings.Contains(ip[idx:], blockArea) {
				continue
			}
			ip = ip[:idx]
		}
		targets = append(targets, ip)
	}

	results := make([]LatencyResult, 0, len(targets))
	resultChan := make(chan LatencyResult, len(targets))
	sem := make(chan struct{}, pingWorkers)
	var wg sync.WaitGroup

	for _, target := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			r := getLatency(ip, 4)
			if r.Latency < defaultLatency {
				resultChan <- r
			}
		}(target)
	}
	wg.Wait()
	close(resultChan)

	for r := range resultChan {
		results = append(results, r)
	}
	return results
}

func getBestIP(list []LatencyResult) string {
	sorted := make([]LatencyResult, len(list))
	copy(sorted, list)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Latency < sorted[j].Latency
	})
	for _, item := range sorted {
		if item.LossRate <= lossThreshold {
			log.Info(fmt.Sprintf("所选ip-%s的丢包率为: %.2f, 延时为: %dms", item.IP, item.LossRate, item.Latency))
			return item.IP
		}
	}
	return ""
}

func newClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

func cfGet(url, email, key string) ([]byte, error) {
	client := newClient()
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/94.0.4606.54 Safari/537.36")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Auth-Email", email)
		req.Header.Set("X-Auth-Key", key)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return body, nil
	}
	return nil, lastErr
}

func uploadIP(ip, name, domain, email, key string) {
	zoneURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?name=%s", domain)
	body, err := cfGet(zoneURL, email, key)
	if err != nil {
		log.Info(fmt.Sprintf("获取 zone 失败: %s", err))
		return
	}
	var zone zoneReply
	if err := json.Unmarshal(body, &zone); err != nil || !zone.Success || len(zone.Results) == 0 {
		log.Info(fmt.Sprintf("获取 %s 失败: 未找到zone", domain))
		return
	}
	zid := zone.Results[0].ID

	recordURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s.%s", zid, name, domain)
	body, err = cfGet(recordURL, email, key)
	if err != nil {
		log.Info(fmt.Sprintf("获取DNS记录失败: %s", err))
		return
	}
	var dns dnsReply
	if err := json.Unmarshal(body, &dns); err != nil || !dns.Success {
		log.Info("获取DNS记录失败: 解析错误")
		return
	}
	rid, proxied := "", false
	for _, record := range dns.Results {
		if record.Type == "A" {
			rid, proxied = record.ID, record.Proxied
			break
		}
	}
	if rid == "" {
		log.Info(fmt.Sprintf("%s.%s 没有A记录, 跳过更新", name, domain))
		return
	}

	params := map[string]interface{}{
		"type":    "A",
		"name":    fmt.Sprintf("%s.%s", name, domain),
		"content": ip,
		"proxied": proxied,
	}
	data, err := json.Marshal(params)
	if err != nil {
		log.Info(fmt.Sprintf("序列化更新参数失败: %s", err))
		return
	}

	updateURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zid, rid)
	client := newClient()
	for attempt := 0; attempt < maxRetries; attempt++ {
		req, err := http.NewRequest("PUT", updateURL, bytes.NewBuffer(data))
		if err != nil {
			log.Info(fmt.Sprintf("构建更新请求失败: %s", err))
			return
		}
		req.Header.Set("X-Auth-Email", email)
		req.Header.Set("X-Auth-Key", key)

		resp, err := client.Do(req)
		if err != nil {
			log.Info(fmt.Sprintf("第%d次更新出错: %s", attempt, err))
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Info(fmt.Sprintf("第%d次读取结果出错: %s", attempt, err))
			continue
		}
		var upd updateReply
		if resp.StatusCode == 200 && json.Unmarshal(body, &upd) == nil && upd.Success {
			log.Info(fmt.Sprintf("成功更新%s.%s的ip为%s", name, domain, ip))
			return
		}
		log.Info(fmt.Sprintf("第%d次更新%s.%s失败, 状态码: %d", attempt, name, domain, resp.StatusCode))
	}
	log.Info(fmt.Sprintf("%s.%s的ip更新失败", name, domain))
}

func handleMain(config Config, domainInfo []string) {
	domain := domainInfo[0]
	root := domainInfo[1]
	blockArea := ""
	if len(domainInfo) >= 3 {
		blockArea = domainInfo[2]
	}
	fullDomain := domain + "." + root

	result := getLatency(fullDomain, 10)
	if result.Latency <= 200 {
		log.Info(fmt.Sprintf("域名%s的ip延时为%dms小于200ms, 未更新", fullDomain, result.Latency))
		return
	}

	if BestIP == "" && len(IPList) == 0 {
		resp, err := http.Get("https://ipdb.api.030101.xyz/?type=bestcf&country=true")
		if err != nil {
			log.Info(fmt.Sprintf("下载优选IP列表错误: %s", err))
			return
		}
		buf, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Info(fmt.Sprintf("读取优选IP列表错误: %s", err))
			return
		}
		IPList = getResultList(string(buf), blockArea)
	}

	if BestIP == "" && len(IPList) > 0 {
		BestIP = getBestIP(IPList)
	}

	if BestIP != "" {
		uploadIP(BestIP, domain, root, config.Email, config.Key)
	}
}

func main() {
	filePath := flag.String("file", "config.json", "文件路径和名称")
	flag.Parse()

	log.SetOutput(os.Stdout)
	log.SetLevel(log.InfoLevel)

	file, err := os.Open(*filePath)
	if err != nil {
		log.Info(fmt.Sprintf("无法打开配置文件: %s", err))
		return
	}
	defer file.Close()

	content, err := io.ReadAll(file)
	if err != nil {
		log.Info(fmt.Sprintf("无法读取配置文件: %s", err))
		return
	}

	var config Config
	if err := json.Unmarshal(content, &config); err != nil {
		log.Info(fmt.Sprintf("无法解析配置文件: %s", err))
		return
	}

	for _, domainInfo := range config.DomainInfos {
		handleMain(config, domainInfo)
	}
}
