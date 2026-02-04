package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/tsnet"
)

// --- HTML TEMPLATE (Browser Fallback) ---
const htmlTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Tail-Burn</title>
    <style>
        body { font-family: -apple-system, system-ui, sans-serif; background: #f4f4f5; display: flex; justify-content: center; align-items: center; height: 100vh; margin: 0; color: #18181b; }
        .card { background: white; padding: 40px; border-radius: 12px; box-shadow: 0 4px 6px -1px rgba(0,0,0,0.1); text-align: center; max-width: 400px; width: 100%; }
        h1 { font-size: 24px; margin-bottom: 10px; }
        p { color: #52525b; margin-bottom: 30px; }
        .file-info { background: #f4f4f5; padding: 15px; border-radius: 8px; margin-bottom: 25px; font-family: monospace; font-size: 14px; text-align: left; }
        .btn { background: #ef4444; color: white; border: none; padding: 12px 24px; border-radius: 6px; font-size: 16px; font-weight: 600; cursor: pointer; width: 100%; }
        .btn:hover { background: #dc2626; }
        .footer { margin-top: 20px; font-size: 12px; color: #a1a1aa; }
        .hidden { display: none; }
    </style>
</head>
<body>
    <div class="card" id="mainCard">
        <h1>🔥 Secure Drop</h1>
        <p><b>{{.Sender}}</b> sent a file.</p>
        <div class="file-info">
            <div>📄 <b>{{.FileName}}</b></div>
            <div>📦 <b>{{.FileSize}}</b></div>
        </div>
        <form method="POST"><button type="submit" class="btn">Download & Destroy</button></form>
        <div class="footer">⚠️ One-time use link.</div>
    </div>
</body>
</html>
`

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "send":
		runSender()
	case "receive":
		runReceiver()
	default:
		printUsage()
	}
}

func printUsage() {
	fmt.Println("Usage:")
	fmt.Println("  tail-burn send -target=<user> <file>   # Host a file")
	fmt.Println("  tail-burn receive <url>                # Download a file")
}

// ==========================================
// SERVER LOGIC (Sender)
// ==========================================
func runSender() {
	sendCmd := flag.NewFlagSet("send", flag.ExitOnError)
	targetUser := sendCmd.String("target", "", "Tailscale login name")
	timeoutMinutes := sendCmd.Int("timeout", 10, "Minutes before auto-burn")
	debugMode := sendCmd.Bool("debug", false, "Enable verbose Tailscale logs") // <--- NEW DEBUG FLAG
	sendCmd.Parse(os.Args[2:])
	filePath := sendCmd.Arg(0)

	if *targetUser == "" || filePath == "" {
		fmt.Println("Usage: tail-burn send -target=<user@provider> <file_path>")
		os.Exit(1)
	}

	// File Prep
	file, err := os.Open(filePath)
	if err != nil {
		log.Fatalf("❌ Error opening file: %v", err)
	}
	defer file.Close()
	stat, _ := file.Stat()
	fileSize := formatBytes(stat.Size())
	fileName := filepath.Base(filePath)

	// Hostname & State
	randSuffix := make([]byte, 2)
	rand.Read(randSuffix)
	hostname := fmt.Sprintf("tail-burn-%x", randSuffix)

	configDir, _ := os.UserConfigDir()
	stateDir := filepath.Join(configDir, "tsnet-"+hostname)

	// --- LOGGING LOGIC ---
	// If -debug is NOT set, we silence the tsnet engine logs
	var tsLogf func(string, ...any)
	if *debugMode {
		tsLogf = log.Printf
	} else {
		tsLogf = func(string, ...any) {} // Silent
	}

	s := &tsnet.Server{
		Hostname:  hostname,
		Dir:       stateDir,
		Ephemeral: true,
		AuthKey:   os.Getenv("TS_AUTHKEY"),
		Logf:      tsLogf, // <--- Apply the conditional logger
	}
	defer func() {
		s.Close()
		os.RemoveAll(stateDir)
	}()

	localClient, err := s.LocalClient()
	if err != nil {
		log.Fatal(err)
	}

	// Generate Secret URL
	randBytes := make([]byte, 12)
	rand.Read(randBytes)
	secretPath := "/" + hex.EncodeToString(randBytes)
	ackPath := secretPath + "/ack" // The "Kill Switch" endpoint

	shutdownSignal := make(chan string) // String for exit reason

	// Handlers
	mux := http.NewServeMux()

	// 1. The ACK Handler (Smart Client Kill Switch)
	mux.HandleFunc(ackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			log.Println("⚡️ ACK received from smart client.")
			w.Write([]byte("OK"))
			shutdownSignal <- "Client confirmed receipt"
		}
	})

	// 2. The Main Handler (Download)
	mux.HandleFunc(secretPath, func(w http.ResponseWriter, r *http.Request) {
		who, err := localClient.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			http.Error(w, "Identity Error", 500)
			return
		}
		if !strings.EqualFold(who.UserProfile.LoginName, *targetUser) {
			log.Printf("⛔️ BLOCKED: %s", who.UserProfile.LoginName)
			http.Error(w, "Forbidden", 403)
			return
		}

		// Detect if it's our smart client
		isSmartClient := r.Header.Get("X-Tail-Burn-Client") == "true"

		if r.Method == "GET" && !isSmartClient {
			// Browser: Show HTML
			sender := "A Tailscale User"
			st, err := localClient.Status(r.Context())
			if err == nil {
				myUserID := st.Self.UserID
				if profile, ok := st.User[myUserID]; ok {
					sender = profile.LoginName
				}
			}

			tmpl, _ := template.New("landing").Parse(htmlTemplate)
			tmpl.Execute(w, struct{ Sender, FileName, FileSize string }{sender, fileName, fileSize})
			return
		}

		if r.Method == "POST" || (r.Method == "GET" && isSmartClient) {
			log.Printf("🚀 Sending file to %s...", who.UserProfile.LoginName)
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))

			if _, err := io.Copy(w, file); err != nil {
				log.Printf("❌ Transfer failed: %v", err)
				return
			}
			if f, ok := w.(http.Flusher); ok { f.Flush() }

			// If it's a browser (POST), we have to guess when to shut down
			if !isSmartClient {
				log.Println("🔥 Browser transfer complete. Starting timer...")
				go func() {
					time.Sleep(5 * time.Second)
					shutdownSignal <- "Browser download finished"
				}()
			}
			// If it's a smart client, we do NOTHING here. We wait for the /ack POST.
		}
	})

	ln, err := s.Listen("tcp", ":80")
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: mux}

	// UI Output
	fmt.Print("\033[H\033[2J")
	fmt.Println("🔥 \033[1mtail-burn\033[0m (Server Mode)")
	fmt.Println("-------------------------------------------")
	fmt.Printf("📦 File: %s (%s)\n", fileName, fileSize)
	fmt.Printf("👤 Target: %s\n", *targetUser)
	fmt.Println("-------------------------------------------")
	url := fmt.Sprintf("http://%s%s", hostname, secretPath)
	fmt.Printf("🌐 Browser Link: \033[32m%s\033[0m\n", url)
	fmt.Printf("💻 Command:      \033[33mtail-burn receive %s\033[0m\n", url)
	fmt.Println("\n(Waiting...)")

	go srv.Serve(ln)

	// Doomsday Timer
	go func() {
		time.Sleep(time.Duration(*timeoutMinutes) * time.Minute)
		shutdownSignal <- "Timeout reached"
	}()

	reason := <-shutdownSignal
	fmt.Printf("\n🛑 Shutting down: %s\n", reason)
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// ==========================================
// CLIENT LOGIC (Receiver)
// ==========================================
func runReceiver() {
	recvCmd := flag.NewFlagSet("receive", flag.ExitOnError)
	recvCmd.Parse(os.Args[2:])
	url := recvCmd.Arg(0)

	if url == "" {
		fmt.Println("Usage: tail-burn receive <url>")
		os.Exit(1)
	}

	fmt.Println("🔍 Connecting to tail-burn server...")

	// 1. Start Download Request
	client := &http.Client{}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-Tail-Burn-Client", "true") // Identify ourselves

	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("❌ Connection failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Fatalf("❌ Server rejected request: HTTP %d", resp.StatusCode)
	}

	// Extract Filename
	contentDisp := resp.Header.Get("Content-Disposition")
	filename := "downloaded_file"
	if strings.Contains(contentDisp, "filename=") {
		parts := strings.Split(contentDisp, "filename=\"")
		if len(parts) > 1 {
			filename = strings.TrimSuffix(parts[1], "\"")
		}
	}
	
	// --- AUTO-RENAME LOGIC ---
	safeName := getSafeFilename(filename)
	if safeName != filename {
		fmt.Printf("⚠️  File '%s' exists. Saving as '%s' instead.\n", filename, safeName)
	}
	filename = safeName
	// -------------------------

	// Create File
	out, err := os.Create(filename)
	if err != nil {
		log.Fatalf("❌ Cannot create file: %v", err)
	}
	defer out.Close()

	// 2. Stream Data
	fmt.Printf("📥 Downloading '%s'...\n", filename)
	size, err := io.Copy(out, resp.Body)
	if err != nil {
		log.Fatalf("❌ Download interrupted: %v", err)
	}
	fmt.Printf("✅ Download complete (%s)\n", formatBytes(size))

	// 3. Send ACK (The Kill Switch)
	fmt.Println("📡 Sending kill signal to server...")
	ackURL := url + "/ack"
	ackResp, err := client.Post(ackURL, "text/plain", nil)
	if err == nil && ackResp.StatusCode == 200 {
		fmt.Println("💥 Server confirmed destruction.")
	} else {
		fmt.Println("⚠️ Server may have already timed out (Link is dead).")
	}
}

// --- HELPER: Find a unique filename (test.bin -> test-1.bin) ---
func getSafeFilename(name string) string {
	// If the file doesn't exist, use the original name
	if _, err := os.Stat(name); os.IsNotExist(err) {
		return name
	}

	// Split "test.bin" into "test" and ".bin"
	var base, ext string
	if idx := strings.LastIndex(name, "."); idx != -1 {
		base = name[:idx]
		ext = name[idx:]
	} else {
		base = name
	}

	// Loop until we find a name that doesn't exist
	for i := 1; ; i++ {
		newName := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(newName); os.IsNotExist(err) {
			return newName
		}
	}
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit { return fmt.Sprintf("%d B", b) }
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
