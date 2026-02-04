# 🔥 tail-burn

![Build Status](https://github.com/scopey/tail-burn/actions/workflows/release.yml/badge.svg)
![Go Version](https://img.shields.io/github/go-mod/go-version/scopey/tail-burn)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**The "Mission Impossible" of file transfers.** Secure, identity-aware, single-use file sharing over [Tailscale](https://tailscale.com).

`tail-burn` spins up a temporary, ephemeral network node inside your process, serves a file **exactly once**, and then destroys itself the moment the transfer is confirmed.

---

## 🚀 Why tail-burn?

- **🔐 Zero Trust:** Validates the Tailscale identity of the downloader. Only the specific user you target can download the file.
- **👻 Ephemeral:** The server process and its network identity exist *only* for the duration of the transfer.
- **💥 Self-Destruct:** The server kills itself immediately after a successful transfer (Client Mode) or after a strict timeout.
- **⚡️ Hybrid Architecture:**
  - **Browser Mode:** Send a link to anyone; they download via a secure HTTPS page.
  - **CLI Mode:** Use the `receive` command for high-speed, automated transfers with instant "Handshake & Burn" logic.
- **🌍 NAT Busting:** Works peer-to-peer across firewalls, NATs, and cafe wifi using WireGuard magic.

---

## 📦 Installation

### Option 1: Binary (Recommended)
Download the latest release for Windows, macOS, or Linux from the [Releases Page](../../releases).

### Option 2: Go Install
```bash
go install github.com/scopey/tail-burn@latest
```

### Option 3: Build from Source
```bash
git clone https://github.com/scopey/tail-burn.git
cd tail-burn
go build -o tail-burn main.go
```

---

## 🛠 Configuration

`tail-burn` needs a **Tailscale Auth Key** to spin up its ephemeral node.

1. Go to **[Tailscale Admin Console > Settings > Keys](https://login.tailscale.com/admin/settings/keys)**.
2. Generate an **Auth Key** (Recommended: *Reusable*, *Ephemeral*, *Pre-approved*).
3. Set it as an environment variable:

**Linux / macOS:**
```bash
export TS_AUTHKEY="tskey-auth-k123456CNTRL-..."
```

**Windows (PowerShell):**
```powershell
$env:TS_AUTHKEY="tskey-auth-k123456CNTRL-..."
```

---

## 🎮 Usage

### 1. Sending a File (Server)
Run this on the machine with the file. It will generate a secure link.

```bash
# Basic usage
tail-burn send -target=user@github ./secret-plans.pdf  ## user@github should be the Tailscale username

# Enable debug logs (noisy)
tail-burn send -debug -target=user@github ./secret-plans.pdf ## user@github should be the Tailscale username
```

*Output:*
```text
🔥 tail-burn (Server Mode)
-------------------------------------------
📦 File: secret-plans.pdf (2.4 MB)
👤 Target: user@github
-------------------------------------------
🌐 Browser Link: https://tail-burn.tailnet-name.ts.net/a1b2c3...
💻 Command:      tail-burn receive https://tail-burn...
```

### 2. Receiving a File (Client)
Run this on the destination machine. It handles the handshake and ensures the server shuts down cleanly.

```bash
tail-burn receive https://tail-burn.tailnet-name.ts.net/a1b2c3...
```

*Features:*
- **Auto-Rename:** If `secret-plans.pdf` exists, it saves as `secret-plans-1.pdf`.
- **Progress Bar:** Clean CLI output.
- **Kill Signal:** Sends a cryptographic ACK to the server upon completion, triggering immediate server destruction.

### 3. Receiving via Browser
Just click the link! 
- You will see a secure landing page verifying the Sender's identity.
- Click "Download & Destroy".
- The server waits 5 seconds after the download finishes to flush buffers, then exits.

---

## 🛡 Security Model

1.  **Identity Verification:** The server uses `localClient.WhoIs()` to cryptographically verify the IP address of the incoming request against the Tailscale coordination server. If the user isn't the target, the connection is dropped immediately (403 Forbidden).
2.  **Traffic Encryption:** All data travels over WireGuard.
3.  **State Cleanup:** The application runs with `Ephemeral: true` (mostly). It attempts to wipe its local state directory on exit to leave no trace of the temporary node key.

---

## 🏗 Development

### Running Tests
We have local test coverage for utility logic (formatting, safe filenames).
```bash
go test -v
```

---

## 📜 License

MIT License. See [LICENSE](LICENSE) for details.
