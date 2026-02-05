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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// --- HTML TEMPLATE (Browser Fallback with UI Fix) ---
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
        .btn { background: #ef4444; color: white; border: none; padding: 12px 24px; border-radius: 6px; font-size: 16px; font-weight: 600; cursor: pointer; width: 100%; transition: background 0.2s; }
        .btn:hover { background: #dc2626; }
        .btn:disabled { background: #a1a1aa; cursor: not-allowed; }
        .footer { margin-top: 20px; font-size: 12px; color: #a1a1aa; }
        .success-icon { font-size: 48px; display: block; margin-bottom: 20px; }
        .hidden { display: none; }
    </style>
    <script>
        function triggerBurn() {
            var btn = document.getElementById('dlBtn');
            var card = document.getElementById('mainContent');
            var done = document.getElementById('doneState');
            
            // 1. Disable button immediately
            btn.disabled = true;
            btn.innerText = "Downloading...";
            
            // 2. Wait 1 second (ensure POST submits), then show Done state
            setTimeout(function() {
                card.classList.add('hidden');
                done.classList.remove('hidden');
            }, 1000);
        }
    </script>
</head>
<body>
    <div class="card">
        <div id="mainContent">
            <h1>🔥 Secure Drop</h1>
            <p><b>{{.Sender}}</b> sent a file.</p>
            <div class="file-info">
                <div>📄 <b>{{.FileName}}</b></div>
                <div>📦 <b>{{.FileSize}}</b></div>
            </div>
            <form method="POST" onsubmit="triggerBurn()">
                <button id="dlBtn" type="submit" class="btn">Download & Destroy</button>
            </form>
            <div class="footer">⚠️ One-time use link.</div>
        </div>

        <div id="doneState" class="hidden">
            <span class="success-icon">💥</span>
            <h1>File Burned</h1>
            <p>The file has been downloaded and the server is self-destructing.</p>
            <div class="footer">You may close this tab.</div>
        </div>
    </div>
</body>
</html>
`

// --- HTML TEMPLATE (Burned Link) ---
const burnedHTMLTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Tail-Burn</title>
    <style>
        body { font-family: -apple-system, system-ui, sans-serif; background: #f4f4f5; display: flex; justify-content: center; align-items: center; height: 100vh; margin: 0; color: #18181b; }
        .card { background: white; padding: 40px; border-radius: 12px; box-shadow: 0 4px 6px -1px rgba(0,0,0,0.1); text-align: center; max-width: 400px; width: 100%; }
        h1 { font-size: 24px; margin-bottom: 10px; }
        p { color: #52525b; margin-bottom: 30px; }
        .success-icon { font-size: 48px; display: block; margin-bottom: 20px; }
        .footer { margin-top: 20px; font-size: 12px; color: #a1a1aa; }
    </style>
</head>
<body>
    <div class="card">
        <span class="success-icon">💥</span>
        <h1>Link Burned</h1>
        <p>This link has already been used or is no longer available.</p>
        <div class="footer">Please request a new link.</div>
    </div>
</body>
</html>
`

type tailBurnClient interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
	Status(ctx context.Context) (*ipnstate.Status, error)
}

var landingTemplate = template.Must(template.New("landing").Parse(htmlTemplate))
var burnedTemplate = template.Must(template.New("burned").Parse(burnedHTMLTemplate))
var browserShutdownDelay = 5 * time.Second

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
	fmt.Println("  tail-burn send -target=<user> [-wipe] <file>   # Host a file")
	fmt.Println("  tail-burn receive <url>                        # Download a file")
}

// ==========================================
// SERVER LOGIC (Sender)
// ==========================================
func runSender() {
	sendCmd := flag.NewFlagSet("send", flag.ExitOnError)
	targetUser := sendCmd.String("target", "", "Tailscale login name")
	timeoutMinutes := sendCmd.Int("timeout", 10, "Minutes before auto-burn")
	debugMode := sendCmd.Bool("debug", false, "Enable verbose Tailscale logs")
	wipe := sendCmd.Bool("wipe", false, "Delete source file after successful transfer")

	sendCmd.Parse(os.Args[2:])
	filePath := sendCmd.Arg(0)

	if *targetUser == "" || filePath == "" {
		fmt.Println("Usage: tail-burn send -target=<user@provider> [-wipe] <file_path>")
		os.Exit(1)
	}

	// File Prep
	stat, err := os.Stat(filePath)
	if err != nil {
		log.Fatalf("❌ Error stating file: %v", err)
	}
	fileSize := formatBytes(stat.Size())
	fileName := filepath.Base(filePath)

	// Hostname & State
	randSuffix := make([]byte, 2)
	if _, err := rand.Read(randSuffix); err != nil {
		log.Fatalf("❌ Error generating hostname suffix: %v", err)
	}
	hostname := fmt.Sprintf("tail-burn-%x", randSuffix)

	configDir, _ := os.UserConfigDir()
	stateDir := filepath.Join(configDir, "tsnet-"+hostname)

	// --- LOGGING LOGIC ---
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
		Logf:      tsLogf,
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
	if _, err := rand.Read(randBytes); err != nil {
		log.Fatalf("❌ Error generating secret path: %v", err)
	}
	secretPath := "/" + hex.EncodeToString(randBytes)
	ackPath := secretPath + "/ack" // The "Kill Switch" endpoint

	shutdownSignal := make(chan string, 1) // Buffered channel to prevent blocking

	// Handlers
	mux := http.NewServeMux()
	registerHandlers(mux, localClient, *targetUser, filePath, fileName, fileSize, shutdownSignal, secretPath, ackPath)

	ln, err := s.Listen("tcp", ":80")
	if err != nil {
		log.Fatal(err)
	}

	// FIX: Timeouts added for security
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// UI Output
	fmt.Print("\033[H\033[2J")
	fmt.Println("🔥 \033[1mtail-burn\033[0m (Server Mode)")
	fmt.Println("-------------------------------------------")
	fmt.Printf("📦 File: %s (%s)\n", fileName, fileSize)
	fmt.Printf("👤 Target: %s\n", *targetUser)
	if *wipe {
		fmt.Println("⚠️  MODE: \033[31mWIPE ENABLED (File will be deleted)\033[0m")
	}
	fmt.Println("-------------------------------------------")
	url := fmt.Sprintf("http://%s%s", hostname, secretPath)
	fmt.Printf("🌐 Browser Link: \033[32m%s\033[0m\n", url)
	fmt.Printf("💻 Command:      \033[33mtail-burn receive %s\033[0m\n", url)
	fmt.Println("\n(Waiting...)")

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("❌ Server error: %v", err)
		}
	}()

	// Doomsday Timer
	go func() {
		time.Sleep(time.Duration(*timeoutMinutes) * time.Minute)
		select {
		case shutdownSignal <- "Timeout reached":
		default:
		}
	}()

	reason := <-shutdownSignal
	fmt.Printf("\n🛑 Shutting down: %s\n", reason)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	// --- WIPE LOGIC RESTORED ---
	if *wipe {
		fmt.Println("🔥 Deleting source file...")
		// We can safely remove because server shutdown ensures file handles are closed
		if err := os.Remove(filePath); err != nil {
			log.Printf("❌ Failed to wipe file: %v", err)
		} else {
			fmt.Println("✅ Source file deleted.")
		}
	}
}

func registerHandlers(
	mux *http.ServeMux,
	localClient tailBurnClient,
	targetUser string,
	filePath string,
	fileName string,
	fileSize string,
	shutdownSignal chan string,
	secretPath string,
	ackPath string,
) {
	var used atomic.Bool
	var inProgress atomic.Bool

	// 1. The ACK Handler (Smart Client Kill Switch)
	mux.HandleFunc(ackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			log.Println("⚡️ ACK received from smart client.")
			w.Write([]byte("OK"))
			used.Store(true)
			select {
			case shutdownSignal <- "Client confirmed receipt":
			default:
			}
		}
	})

	// 2. The Main Handler (Download)
	mux.HandleFunc(secretPath, func(w http.ResponseWriter, r *http.Request) {
		who, err := localClient.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			http.Error(w, "Identity Error", 500)
			return
		}
		if !strings.EqualFold(who.UserProfile.LoginName, targetUser) {
			log.Printf("⛔️ BLOCKED: %s", who.UserProfile.LoginName)
			http.Error(w, "Forbidden", 403)
			return
		}

		// Detect if it's our smart client
		isSmartClient := r.Header.Get("X-Tail-Burn-Client") == "true"

		if used.Load() {
			if !isSmartClient {
				w.WriteHeader(http.StatusGone)
				_ = burnedTemplate.Execute(w, nil)
				return
			}
			http.Error(w, "Gone", http.StatusGone)
			return
		}

		if r.Method == "GET" && !isSmartClient {
			// Browser: Show HTML
			sender := "A Tailscale User"
			st, err := localClient.Status(r.Context())
			if err == nil && st != nil && st.Self != nil {
				myUserID := st.Self.UserID
				if profile, ok := st.User[myUserID]; ok {
					sender = profile.LoginName
				}
			}

			if err := landingTemplate.Execute(w, struct{ Sender, FileName, FileSize string }{sender, fileName, fileSize}); err != nil {
				http.Error(w, "Template Error", http.StatusInternalServerError)
				return
			}
			return
		}

		if r.Method == "POST" || (r.Method == "GET" && isSmartClient) {
			if !inProgress.CompareAndSwap(false, true) {
				if !isSmartClient {
					w.WriteHeader(http.StatusGone)
					_ = burnedTemplate.Execute(w, nil)
					return
				}
				http.Error(w, "Gone", http.StatusGone)
				return
			}
			success := false
			defer func() {
				if !success {
					inProgress.Store(false)
				}
			}()

			log.Printf("🚀 Sending file to %s...", who.UserProfile.LoginName)

			// Open file fresh for every request
			file, err := os.Open(filePath)
			if err != nil {
				http.Error(w, "File Error", http.StatusInternalServerError)
				return
			}
			defer file.Close()

			fi, err := file.Stat()
			if err != nil {
				http.Error(w, "File Error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))

			if _, err := io.Copy(w, file); err != nil {
				log.Printf("❌ Transfer failed: %v", err)
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}

			inProgress.Store(false)
			success = true

			// If it's a browser (POST), we have to guess when to shut down
			if !isSmartClient {
				used.Store(true)
				log.Println("🔥 Browser transfer complete. Starting timer...")
				go func() {
					time.Sleep(browserShutdownDelay)
					select {
					case shutdownSignal <- "Browser download finished":
					default:
					}
				}()
			}
			// If it's a smart client, we do NOTHING here. We wait for the /ack POST.
		}
	})
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

	if err := receive(url); err != nil {
		log.Fatalf("❌ %v", err)
	}
}

func receive(url string) error {
	fmt.Println("🔍 Connecting to tail-burn server...")

	// 1. Start Download Request
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("bad request URL: %w", err)
	}
	req.Header.Set("X-Tail-Burn-Client", "true") // Identify ourselves

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("server rejected request: HTTP %d", resp.StatusCode)
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
		return fmt.Errorf("cannot create file: %w", err)
	}
	defer out.Close()

	// 2. Stream Data
	fmt.Printf("📥 Downloading '%s'...\n", filename)
	size, err := io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("download interrupted: %w", err)
	}
	// FIX: Check Content-Length integrity
	if resp.ContentLength > 0 && size != resp.ContentLength {
		return fmt.Errorf("download incomplete: expected %d bytes, got %d", resp.ContentLength, size)
	}
	fmt.Printf("✅ Download complete (%s)\n", formatBytes(size))

	// 3. Send ACK (The Kill Switch)
	fmt.Println("📡 Sending kill signal to server...")
	ackURL := url + "/ack"
	ackResp, err := client.Post(ackURL, "text/plain", nil)
	if err == nil {
		defer ackResp.Body.Close()
		if ackResp.StatusCode == 200 {
			fmt.Println("💥 Server confirmed destruction.")
		} else {
			fmt.Println("⚠️ Server responded but did not confirm destruction.")
		}
	} else {
		fmt.Println("⚠️ Server may have already timed out (Link is dead).")
	}
	return nil
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
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
