package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Local fixture only: verifies CDP response decoding across the page navigation
// without sending any real email or reading production credentials.
func TestBrowserOTPCaptureLocal(t *testing.T) {
	if os.Getenv("NEXORA_TEST_CHROME") != "1" {
		t.Skip("set NEXORA_TEST_CHROME=1 for the local browser fixture")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/send-otp" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"page":{"type":"email_otp_verification"},"oai-client-auth-session":{"email":"alias@icloud.com","passwordless_email_otp_send_pending":false}}`))
			return
		}
		if r.URL.Path == "/email-verification" {
			_, _ = w.Write([]byte(`<html>Verification</html>`))
			return
		}
		_, _ = w.Write([]byte(`<html><script>setTimeout(()=>fetch('/send-otp',{method:'POST'}).then(()=>location.href='/email-verification'),200)</script></html>`))
	}))
	defer server.Close()
	chrome, err := findSentinelChrome()
	if err != nil {
		t.Fatal(err)
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.ExecPath(chrome), chromedp.Flag("headless", "new"))
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	page, cancelPage := chromedp.NewContext(alloc)
	defer cancelPage()
	if err := chromedp.Do(page); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(page, 20*time.Second)
	defer cancel()
	if err := retainRegisterBrowserResponses(ctx); err != nil {
		t.Fatal(err)
	}
	results := make(chan registerBrowserOTPResponse, 1)
	events := chromedp.Events(ctx, network.ResponseReceived)
	go func() {
		for e, err := range events {
			if err != nil {
				return
			}
			if strings.HasSuffix(e.Response.URL, "/send-otp") {
				results <- readRegisterBrowserOTPResponse(ctx, e.RequestID, e.Response.Status, "alias@icloud.com")
				return
			}
		}
	}()
	if err := chromedp.Do(ctx, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-results:
		if err := r.validate(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("no fixture response captured")
	}
}

func TestBrowserForgotPasswordLocal(t *testing.T) {
	if os.Getenv("NEXORA_TEST_CHROME") != "1" {
		t.Skip("set NEXORA_TEST_CHROME=1 for local browser fixture")
	}
	for _, confirmOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmOnly), func(t *testing.T) {
			var submitted atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				switch r.URL.Path {
				case "/favicon.ico":
					w.WriteHeader(http.StatusNoContent)
				case "/log-in/password":
					fmt.Fprint(w, `<html><input type="password"><a href="/reset-password">Forgot password?</a></html>`)
				case "/reset-password":
					if confirmOnly {
						fmt.Fprint(w, `<html><form action="/email-verification"><button type="submit" formaction="/cancel">Cancel</button><button type="submit">Continue</button></form></html>`)
					} else {
						fmt.Fprint(w, `<html><form action="/email-verification"><input type="email" name="email" required><button type="submit">Send code</button></form></html>`)
					}
				case "/email-verification":
					if confirmOnly || r.URL.Query().Get("email") == "fixture@example.com" {
						submitted.Add(1)
					}
					fmt.Fprint(w, `<html><input name="code"></html>`)
				default:
					t.Errorf("unexpected reset navigation: %s", r.URL.Path)
				}
			}))
			defer server.Close()
			chrome, err := findSentinelChrome()
			if err != nil {
				t.Fatal(err)
			}
			opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
			opts = append(opts, chromedp.ExecPath(chrome), chromedp.Flag("headless", "new"), chromedp.Flag("ignore-certificate-errors", true), chromedp.Flag("host-resolver-rules", "MAP auth.openai.com 127.0.0.1"), chromedp.Flag("no-proxy-server", true))
			alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
			defer cancelAlloc()
			page, cancelPage := chromedp.NewContext(alloc)
			defer cancelPage()
			ctx, cancel := context.WithTimeout(page, 30*time.Second)
			defer cancel()
			u, _ := url.Parse(server.URL)
			u.Host = "auth.openai.com:" + u.Port()
			u.Path = "/log-in/password"
			if err := chromedp.Do(ctx, chromedp.Navigate(u.String())); err != nil {
				t.Fatal(err)
			}
			flow := &registerPasswordBrowserFlow{ctx: ctx, email: "fixture@example.com"}
			if err := flow.waitEmailVerification(); err != nil {
				t.Fatal(err)
			}
			if submitted.Load() != 1 || flow.cutoff.IsZero() {
				t.Fatal("reset did not submit exactly once with a fresh OTP boundary")
			}
		})
	}
}

func TestBrowserPasswordFormLocal(t *testing.T) {
	if os.Getenv("NEXORA_TEST_CHROME") != "1" {
		t.Skip("set NEXORA_TEST_CHROME=1 for the local browser fixture")
	}
	submitted := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/password/add" {
			var body map[string]string
			err := json.NewDecoder(r.Body).Decode(&body)
			submitted <- err == nil && body["password"] == "fixturePassword123!" && body["confirmation"] == body["password"] && body["honeypot"] == ""
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><form id="passwordForm"><input name="honeypot" type="password" style="display:none"><input id="p" name="password" type="password" minlength="12" required><input id="c" name="confirmation" type="password" required><button type="submit" disabled>Save password</button></form><script>
  const f=document.getElementById('passwordForm'),p=document.getElementById('p'),c=document.getElementById('c'),b=f.querySelector('button');
  f.addEventListener('input',()=>b.disabled=!(p.value.length>=12&&p.value===c.value));
  f.addEventListener('submit',e=>{e.preventDefault();fetch('/password/add',{method:'POST',body:JSON.stringify({password:p.value,confirmation:c.value,honeypot:f.elements.honeypot.value})})});
  </script></html>`))
	}))
	defer server.Close()
	chrome, err := findSentinelChrome()
	if err != nil {
		t.Fatal(err)
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.ExecPath(chrome), chromedp.Flag("headless", "new"))
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	page, cancelPage := chromedp.NewContext(alloc)
	defer cancelPage()
	if err := chromedp.Do(page); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(page, 20*time.Second)
	defer cancel()
	if err := chromedp.Do(ctx, chromedp.Navigate(server.URL)); err != nil {
		t.Fatal(err)
	}
	if err := fillRegisterBrowserPassword(ctx, "fixturePassword123!"); err != nil {
		t.Fatal(err)
	}
	if err := submitRegisterBrowserPassword(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-submitted:
		if !ok {
			t.Fatal("visible password fields were not submitted correctly")
		}
	case <-ctx.Done():
		t.Fatal("password form never submitted")
	}
}

// Exercises the production step and CDP observer against a local auth fixture:
// manual submit, page auto-submit and explicit upstream rejection.
func TestBrowserCodeFormLocal(t *testing.T) {
	if os.Getenv("NEXORA_TEST_CHROME") != "1" {
		t.Skip("set NEXORA_TEST_CHROME=1 for the local browser fixture")
	}
	for _, mode := range []string{"manual", "automatic", "rejected", "same-step"} {
		t.Run(mode, func(t *testing.T) {
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/accounts/email-otp/validate" {
					count.Add(1)
					var body map[string]string
					if json.NewDecoder(r.Body).Decode(&body) != nil || body["code"] != "123456" {
						t.Error("wrong fixture code submitted")
					}
					w.Header().Set("Content-Type", "application/json")
					switch mode {
					case "rejected":
						w.WriteHeader(400)
						_, _ = w.Write([]byte(`{"error":{"code":"invalid_otp"}}`))
					case "same-step":
						_, _ = w.Write([]byte(`{"page":{"type":"email_otp_verification"},"continue_url":"/email-verification"}`))
					default:
						_, _ = w.Write([]byte(`{"page":{"type":"about_you"},"continue_url":"/about-you"}`))
					}
					return
				}
				w.Header().Set("Content-Type", "text/html")
				if r.URL.Path == "/about-you" {
					_, _ = w.Write([]byte(`<html>profile</html>`))
					return
				}
				_, _ = fmt.Fprintf(w, `<html><form><input type="email" style="display:none"><button type="submit" onclick="throw Error('wrong form')">Other form</button></form>
    <form id="otp"><input name="code" style="display:none" disabled><input id="code" name="code" inputmode="numeric" value="999999"><button type="submit" name="intent">Continue</button></form>
    <script>const f=document.querySelector('#otp'),c=document.querySelector('#code');let sent=false;
    async function send(){if(sent)return;sent=true;const r=await fetch('/api/accounts/email-otp/validate',{method:'POST',body:JSON.stringify({code:c.value})});if(r.ok&&%q!=='same-step')location.href='/about-you'}
    f.addEventListener('submit',e=>{e.preventDefault();send()});
    c.addEventListener('input',()=>{if(%q==='automatic'&&c.value.length===6)send()});
    </script></html>`, mode, mode)
			}))
			defer server.Close()
			chrome, err := findSentinelChrome()
			if err != nil {
				t.Fatal(err)
			}
			opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
			opts = append(opts, chromedp.ExecPath(chrome), chromedp.Flag("headless", "new"))
			alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
			defer cancelAlloc()
			page, cancelPage := chromedp.NewContext(alloc)
			defer cancelPage()
			if err := chromedp.Do(page); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(page, 20*time.Second)
			defer cancel()
			trace, err := observeRegisterBrowserPaths(ctx, "fixture@icloud.com", func(raw string) string {
				u, err := url.Parse(raw)
				if err != nil {
					return ""
				}
				u.Host = "auth.openai.com"
				return registerBrowserAccountPath(u.String())
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := chromedp.Do(ctx, chromedp.Navigate(server.URL+"/email-verification")); err != nil {
				t.Fatal(err)
			}
			err = validateRegisterBrowserCode(ctx, trace, "123456")
			wantError := mode == "rejected" || mode == "same-step"
			if (err != nil) != wantError {
				t.Fatalf("unexpected result: %v", err)
			}
			if mode == "rejected" && !strings.Contains(err.Error(), "invalid_otp") {
				t.Fatal(err)
			}
			if count.Load() != 1 {
				t.Fatalf("OTP submitted %d times", count.Load())
			}
		})
	}
}
