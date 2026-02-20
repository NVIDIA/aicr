#!/bin/bash
# Dynamo Chat UI — single script to launch everything
# Usage: ./chat-server.sh
# Then open: http://127.0.0.1:9090/chat.html

set -e

NAMESPACE="${NAMESPACE:-dynamo-workload}"
SERVICE="${SERVICE:-svc/vllm-agg-frontend}"
API_PORT=8000
UI_PORT=9090

cleanup() {
    echo "Shutting down..."
    kill $PF_PID 2>/dev/null
    kill $PY_PID 2>/dev/null
    exit 0
}
trap cleanup EXIT INT TERM

# Kill anything already on our ports
for port in $API_PORT $UI_PORT; do
    pids=$(lsof -ti :$port 2>/dev/null || true)
    if [ -n "$pids" ]; then
        echo "Killing existing processes on port $port"
        echo "$pids" | xargs kill 2>/dev/null || true
        sleep 1
    fi
done

# Start port-forward to Dynamo frontend
echo "Starting port-forward to $SERVICE on :$API_PORT..."
kubectl port-forward -n "$NAMESPACE" "$SERVICE" "$API_PORT":8000 &
PF_PID=$!
sleep 2

# Start chat UI + API proxy on UI_PORT
echo "Starting chat UI on :$UI_PORT..."
python3 -c "
import http.server, urllib.request, io

API = 'http://127.0.0.1:${API_PORT}'
HTML_PATH = '$(dirname "$0")/chat.html'

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/' or self.path == '/chat.html':
            html = open(HTML_PATH, 'rb').read() if __import__('os').path.exists(HTML_PATH) else b''
            self.send_response(200)
            self.send_header('Content-Type', 'text/html')
            self.send_header('Content-Length', len(html))
            self.end_headers()
            self.wfile.write(html)
        elif self.path.startswith('/v1/'):
            self._proxy()
        else:
            self.send_error(404)

    def do_POST(self):
        if self.path.startswith('/v1/'):
            self._proxy()
        else:
            self.send_error(404)

    def _proxy(self):
        length = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(length) if length else None
        req = urllib.request.Request(
            API + self.path, data=body,
            headers={'Content-Type': self.headers.get('Content-Type', 'application/json')},
            method=self.command)
        try:
            with urllib.request.urlopen(req) as r:
                data = r.read()
                self.send_response(r.status)
                self.send_header('Content-Type', r.headers.get('Content-Type', 'application/json'))
                self.send_header('Content-Length', len(data))
                self.end_headers()
                self.wfile.write(data)
        except urllib.error.URLError as e:
            self.send_error(502, str(e))

    def log_message(self, fmt, *args): pass

http.server.HTTPServer(('127.0.0.1', ${UI_PORT}), H).serve_forever()
" &
PY_PID=$!

echo ""
echo "Ready! Open http://127.0.0.1:${UI_PORT}/chat.html"
echo "Press Ctrl+C to stop."
echo ""

wait
