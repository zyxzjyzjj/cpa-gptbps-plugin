// bps-login exports an independent BPS OAuth grant using the original browser
// callback interception. It never reads Codex credentials or browser profiles.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/zyxzjyzjj/cpa-gptbps-plugin/internal/auth"
)

func main() {
	output := flag.String("out", "", "New private BPS OAuth JSON path (must not exist)")
	proxy := flag.String("proxy", "", "Explicit proxy used by browser and token exchange")
	browser := flag.String("browser-path", "", "Optional Chromium executable")
	method := flag.String("method", "browser", "browser or device")
	cdp := flag.String("cdp-page", "", "Existing task-owned loopback page CDP WebSocket URL")
	flag.Parse()
	if *output == "" {
		fmt.Fprintln(os.Stderr, "--out is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	client, err := auth.NewBrowserClient(*proxy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Invalid proxy configuration")
		os.Exit(2)
	}
	var creds *auth.DeviceCredentials
	switch *method {
	case "browser":
		creds, err = client.LoginBrowser(ctx, func(ctx context.Context, authorizeURL string) (string, error) {
			if *cdp != "" {
				return auth.CaptureBrowserAuthorizationInTab(ctx, authorizeURL, *cdp)
			}
			return auth.CaptureBrowserAuthorization(ctx, authorizeURL, *proxy, *browser)
		})
	case "device":
		creds, err = client.Login(ctx, func(prompt auth.DevicePrompt) error {
			fmt.Printf("Open: %s\nDevice code: %s\n", prompt.URL, prompt.UserCode)
			return nil
		})
	default:
		fmt.Fprintln(os.Stderr, "--method must be browser or device")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "BPS authorization failed:", err)
		os.Exit(1)
	}
	if err = auth.SaveDeviceCredentials(*output, creds); err != nil {
		fmt.Fprintln(os.Stderr, "Could not publish a new OAuth file; existing files are never replaced")
		os.Exit(1)
	}
	fmt.Println("BPS authorization saved. Import the JSON using the Codex BPS panel in CPA.")
}
