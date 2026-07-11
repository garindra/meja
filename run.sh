#sudo ./bin/tali-server -listen :4433 -cert ./dev/server.crt -key ./dev/server.key
sudo ./bin/tali-server \
  -listen :4433 \
  -cert ./dev/server.crt \
  -key ./dev/server.key \
  -terminal-debug-log /tmp/tali-terminal.log