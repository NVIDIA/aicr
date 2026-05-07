#!/bin/bash
# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# NIM Chat UI — single script to launch everything
# Usage: ./nim-chat-server.sh
# Then open: http://127.0.0.1:9090/chat.html

set -e

NAMESPACE="${NAMESPACE:-nim-workload}"
SERVICE="${SERVICE:-svc/llama-3-2-1b}"
API_PORT="${API_PORT:-8000}"
UI_PORT="${UI_PORT:-9090}"

cleanup() {
    echo "Shutting down..."
    kill $PF_PID 2>/dev/null
    kill $PY_PID 2>/dev/null
    exit 0
}
trap cleanup EXIT INT TERM

# Check if our ports are already in use
for port in $API_PORT $UI_PORT; do
    if lsof -ti :$port &>/dev/null; then
        echo "Error: port $port is already in use. Free it or set a different port:"
        echo "  UI_PORT=9091 API_PORT=8001 $0"
        lsof -ti :$port 2>/dev/null | xargs ps -p 2>/dev/null | tail -1
        exit 1
    fi
done

# Start port-forward to NIM service
echo "Starting port-forward to $SERVICE on :$API_PORT..."
kubectl port-forward -n "$NAMESPACE" "$SERVICE" "$API_PORT":8000 &
PF_PID=$!
sleep 2

# Verify port-forward is still running
if ! kill -0 $PF_PID 2>/dev/null; then
    echo "Error: port-forward to $SERVICE failed. Check that the service exists:"
    echo "  kubectl get svc -n $NAMESPACE"
    exit 1
fi


# Start chat UI + API proxy on UI_PORT
echo "Starting chat UI on :$UI_PORT..."
python3 -c "
import http.server, urllib.request, io

API = 'http://127.0.0.1:${API_PORT}'
HTML_PATH = '$(dirname "$0")/nim-chat.html'

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
