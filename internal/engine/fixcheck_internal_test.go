package engine

import "testing"

// A request only counts in a request line's own form, anchored at the start.
func TestRequestOf(t *testing.T) {
	tests := []struct {
		line   string
		key    string
		status int
	}{
		// The forms dev servers print.
		{"GET /notices 500 in 30ms", "GET /notices", 500},
		{" GET /notices?page=2 200 in 12ms", "GET /notices?page=2", 200},
		{"[info] GET /notices", "GET /notices", 0},
		{"10:41:03.120 [info] POST /notices", "POST /notices", 0},
		{"GET /notices 500 12.345 ms - 123", "GET /notices", 500},
		{`Started GET "/notices?page=2" for 127.0.0.1 at 2026-01-01 09:00:00 +0000`, "GET /notices?page=2", 0},
		{`[abc-123] Started DELETE "/notices/1" for 127.0.0.1`, "DELETE /notices/1", 0},
		{`127.0.0.1 - - [01/Jan/2026:09:00:00 +0000] "GET /notices HTTP/1.1" 500 512`, "GET /notices", 500},
		{`INFO:     127.0.0.1:52342 - "GET /notices HTTP/1.1" 500 Internal Server Error`, "GET /notices", 500},
		{`[01/Jan/2026 09:00:00] "GET /notices HTTP/1.1" 500 145`, "GET /notices", 500},
		{`127.0.0.1 - - [01/Jan/2026 09:00:00] "GET /notices HTTP/1.1" 500 -`, "GET /notices", 500},
		// morgan's short format isn't read: anything could come before its
		// method, as a client chooses the user.
		{"::1 - GET /notices?page=2 HTTP/1.1 500 12 - 3.1 ms", "", 0},
		// Only HTTP/1.x and 0.9: Python's http.server refuses later versions
		// itself, before the app runs.
		{`127.0.0.1 - - [01/Jan/2026 09:00:00] "GET /notices HTTP/2.0" 505 -`, "", 0},
		// Tags have no spaces, so a tag can't hold a request of its own.
		{"[x GET /notices 200] y", "", 0},
		// A request mentioned anywhere else on a line isn't one.
		{"Searching for GET /notices 200", "", 0},
		{`  Parameters: {"q"=>"x GET /notices HTTP/1.1 200"}`, "", 0},
		{"Search query: GET /notices HTTP/1.1 200", "", 0},
		// Werkzeug and Django log a bad request line raw, quotes and all.
		{`127.0.0.1 - - [01/Jan/2026 09:00:00] "GET /notices HTTP/1.1" 200" 400 -`, "", 0},
		{`[01/Jan/2026 09:00:00] "GET /notices HTTP/1.1" 200 5" 400 0`, "", 0},
		// A user or forwarded address with spaces in it.
		{`127.0.0.1 - x [d] "GET /notices HTTP/1.1" 200 y [01/Jan/2026:09:00:00 +0000] "GET /x HTTP/1.1" 401 0`, "", 0},
		{`INFO:     1.2.3.4 - "GET /notices HTTP/1.1" 200 OK x:0 - "GET /x" 404 Not Found`, "", 0},
		{`  Parameters: {"q"=>"GET /notices 200"}`, "", 0},
		{"Not Found: /x GET /notices 200", "", 0},
		{`No route matches [GET] "/notices"`, "", 0},
		{`Started GET "/x?q=GET+/notices+200" for 127.0.0.1`, "GET /x?q=GET+/notices+200", 0},
		{`Processing by NoticesController#index as HTML GET /notices 200`, "", 0},
		{`127.0.0.1 - - [01/Jan/2026:09:00:00 +0000] "GET /x?q=%22GET%20/notices%20HTTP/1.1%22%20200 HTTP/1.1" 404 0`, "", 0},
		{"Пользователь GET /notices 200", "", 0},
		{"PURGE /notices 200", "", 0},
	}
	// foreman's prefix comes off first, as it does in the fix check.
	tests = append(tests, struct {
		line   string
		key    string
		status int
	}{`10:41:03 web.1  | Started GET "/notices" for 127.0.0.1`, "GET /notices", 0})
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			key, status := requestOf(unprefix(tt.line))
			if key != tt.key || status != tt.status {
				t.Fatalf("requestOf = %q %d, want %q %d", key, status, tt.key, tt.status)
			}
		})
	}
}

// Any method token makes a line a request, so it can't be a signal.
func TestMentionsRequest(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"GET /notices 200", true},
		{"PURGE /x 404", true},
		{"M-SEARCH /x 404", true},
		{"compiled-successfully - PURGE2 /x", true},
		{`Started PROPFIND "/x" for 127.0.0.1`, true},
		{`x "SEARCH / HTTP/1.1" 404`, true},
		{`x "purge /x HTTP/1.1" 400`, true},
		{`x "f.o * HTTP/1.0" 400`, true},
		{" ✓ Compiled /notices in 312ms", false},
		{"10:41:03 AM [vite] hmr update /src/App.tsx", false},
		{"/app/notices/views.py changed, reloading.", false},
		{"PASS src/notices.test.ts", false},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			if got := mentionsRequest.MatchString(tt.line); got != tt.want {
				t.Fatalf("mentionsRequest = %v, want %v", got, tt.want)
			}
		})
	}
}
