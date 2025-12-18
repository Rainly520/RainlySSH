package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
var wsMutex sync.Mutex

// 消息结构体（与前端对齐）
type WSMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// 资源信息结构体
type ResourceInfo struct {
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	NetRecv float64 `json:"netRecv"`
	NetSend float64 `json:"netSend"`
}

// 进程信息结构体（新增）
type ProcessInfo struct {
	Pid  int32   `json:"pid"`  // 进程ID
	Name string  `json:"name"` // 进程名
	CPU  float64 `json:"cpu"`  // CPU使用率（%）
	Mem  float64 `json:"mem"`  // 内存使用率（%）
	Cmd  string  `json:"cmd"`  // 启动命令
}

func main() {
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()
	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"http://localhost:5173", "https://ssh.rainly.net"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{
		"Origin", 
		"Content-Type", 
		"Accept", 
		"X-Requested-With",
		"Authorization",
		"Cookie",
		},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
		ExposeHeaders:    []string{"Set-Cookie", "Content-Length"},
		AllowWildcard:    false,
	}))

	router.StaticFile("/", "./dist/index.html") 
    router.Static("/assets", "./dist/assets")
    router.Use(func(c *gin.Context) {
      if strings.HasPrefix(c.Request.URL.Path, "/assets") {
        c.Header("Cache-Control", "public, max-age=86400") // 缓存 1 天
      }
      c.Next()
    })

	router.GET("/api/ssh", sshWSHandler)
	log.Fatal(router.Run(":8667"))
}

// SSH WebSocket
func sshWSHandler(c *gin.Context) {
	host := c.Query("host")
	port, _ := strconv.Atoi(c.Query("port"))
	if port == 0 {
		port = 22
	}
	username := c.Query("username")
	password := c.Query("password")

	// 升级HTTP为WebSocket
	wsConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("WebSocket升级失败:", err)
		return
	}
	defer wsConn.Close()

	// 构建SSH客户端配置
	sshConfig := &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // 生产环境建议替换为安全验证
		Timeout:         10 * time.Second,
	}

	// 连接目标SSH服务器
	sshClient, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", host, port), sshConfig)
	if err != nil {
		sendMsg(wsConn, "error", "SSH连接失败: "+err.Error())
		return
	}
	defer sshClient.Close()

	// 创建SSH终端会话
	session, err := sshClient.NewSession()
	if err != nil {
		sendMsg(wsConn, "error", "创建终端会话失败: "+err.Error())
		return
	}
	defer session.Close()

	// 配置PTY伪终端（确保终端交互正常）
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,     // 开启输入回显
		ssh.ICRNL:         1,     // 回车转换行（解决输入回车无响应）
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm", 40, 120, modes); err != nil {
		sendMsg(wsConn, "error", "请求终端失败: "+err.Error())
		return
	}

	// 获取终端IO管道（核心交互链路）
	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()
	stderr, _ := session.StderrPipe()

	// 启动Shell进入交互模式
	if err := session.Shell(); err != nil {
		sendMsg(wsConn, "error", "启动终端Shell失败: "+err.Error())
		return
	}

	// 转发SSH输出到前端终端
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stdout.Read(buf)
			if err != nil {
				break
			}
			sendMsg(wsConn, "output", string(buf[:n]))
		}
	}()
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stderr.Read(buf)
			if err != nil {
				break
			}
			sendMsg(wsConn, "output", string(buf[:n]))
		}
	}()

	// 转发前端输入到SSH终端
	go func() {
		for {
			_, msgBytes, err := wsConn.ReadMessage()
			if err != nil {
				break
			}
			var msg struct{ Type, Data string }
			json.Unmarshal(msgBytes, &msg)
			if msg.Type == "cmd" {
				stdin.Write([]byte(msg.Data))
			}
		}
	}()

	// 启动资源采集协程
	go collectResource(wsConn, sshClient)
	// 启动进程采集协程（新增）
	go collectProcesses(wsConn, sshClient)

	// 阻塞保持会话连接
	session.Wait()
}

// 采集服务器资源信息
func collectResource(wsConn *websocket.Conn, sshClient *ssh.Client) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// 初始化网络接口与带宽数据
	var lastRecv, lastSend uint64
	netIface := getNetIface(sshClient)
	bandCmd := fmt.Sprintf("cat /proc/net/dev | grep %s | awk '{print $2, $10}'", netIface)

	for range ticker.C {
		res := ResourceInfo{}

		// 采集CPU使用率
		cpuCmd := `top -bn1 | grep '%Cpu' | awk -F',' '{for(i=1;i<=NF;i++){if($i ~ /id/){split($i,a," ");id=a[1];break}}} END {print 100 - id}'`
		cpuOut, _ := execCmd(sshClient, cpuCmd)
		res.CPU, _ = strconv.ParseFloat(strings.TrimSpace(cpuOut), 64)
		if res.CPU < 0 || res.CPU > 100 {
			res.CPU = 0
		}

		// 采集内存使用率
		memCmd := `free | grep Mem | awk '{print $3/$2*100}'`
		memOut, _ := execCmd(sshClient, memCmd)
		res.Mem, _ = strconv.ParseFloat(strings.TrimSpace(memOut), 64)

		// 采集带宽
		bandOut, _ := execCmd(sshClient, bandCmd)
		parts := strings.Fields(bandOut)
		if len(parts) >= 2 {
			currentRecv, _ := strconv.ParseUint(parts[0], 10, 64)
			currentSend, _ := strconv.ParseUint(parts[1], 10, 64)
			if lastRecv > 0 {
				res.NetRecv = float64(currentRecv-lastRecv) / 1024 / 2
				res.NetSend = float64(currentSend-lastSend) / 1024 / 2
			}
			lastRecv, lastSend = currentRecv, currentSend
		}

		sendMsg(wsConn, "resource", res)
	}
}

// 采集进程信息
func collectProcesses(wsConn *websocket.Conn, sshClient *ssh.Client) {
	ticker := time.NewTicker(3 * time.Second) // 3秒刷新一次进程
	defer ticker.Stop()

	for range ticker.C {
		// 执行ps命令获取进程（按CPU排序，取前50条）
		psCmd := `ps aux --sort=-%cpu | head -51 | awk 'NR>1 {print $2, $11, $3, $4, substr($0, index($0,$5))}'`
		psOut, err := execCmd(sshClient, psCmd)
		if err != nil {
			log.Printf("采集进程失败: %v", err)
			continue
		}

		var processes []ProcessInfo
		lines := strings.Split(psOut, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			parts := strings.Fields(line)
			if len(parts) < 5 {
				continue
			}

			// 解析进程字段
			pid, _ := strconv.ParseInt(parts[0], 10, 32)
			cpu, _ := strconv.ParseFloat(parts[2], 64)
			mem, _ := strconv.ParseFloat(parts[3], 64)

			processes = append(processes, ProcessInfo{
				Pid:  int32(pid),
				Name: parts[1],
				CPU:  cpu,
				Mem:  mem,
				Cmd:  strings.Join(parts[4:], " "),
			})
		}

		// 发送进程数据到前端
		sendMsg(wsConn, "process", processes)
	}
}

// 执行SSH命令（资源/进程采集通用）
func execCmd(sshClient *ssh.Client, cmd string) (string, error) {
	session, err := sshClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("创建命令会话失败: %v", err)
	}
	defer session.Close()
	out, err := session.CombinedOutput(cmd)
	return string(out), err
}

// 获取目标服务器的网络接口
func getNetIface(sshClient *ssh.Client) string {
	out, _ := execCmd(sshClient, `ip link show | grep UP | awk '{print $2}' | cut -d: -f1 | head -1`)
	if iface := strings.TrimSpace(out); iface != "" {
		return iface
	}
	return "eth0"
}

// 发送WebSocket消息（线程安全）
func sendMsg(wsConn *websocket.Conn, typ string, data interface{}) {
	wsMutex.Lock()
	defer wsMutex.Unlock()

	dataBytes, _ := json.Marshal(data)
	msg := WSMessage{Type: typ, Data: dataBytes}
	msgBytes, _ := json.Marshal(msg)
	wsConn.WriteMessage(websocket.TextMessage, msgBytes)
}