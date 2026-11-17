# NixOS VM test: the module starts the service with a hardened unit, the
# router answers its own endpoints, and a systemd-driver switch restarts a
# backend unit through polkit.
{
  self,
  pkgs,
}:
pkgs.testers.runNixOSTest {
  name = "model-router";
  nodes.machine = {pkgs, ...}: {
    imports = [self.nixosModules.default];

    # A stand-in backend: serves the selected variant on /v1/models.
    systemd.services.llm-backend = {
      wantedBy = ["multi-user.target"];
      serviceConfig.ExecStart = pkgs.writeShellScript "fake-backend" ''
        variant="$(cat /var/lib/model-router/variant 2>/dev/null || echo large)"
        exec ${pkgs.python3}/bin/python3 -c '
        import http.server, json, sys
        variant = sys.argv[1]
        class H(http.server.BaseHTTPRequestHandler):
            def reply(self):
                body = json.dumps({"data": [{"id": variant, "status": {"value": "loaded"}}], "variant": variant}).encode()
                self.send_response(200); self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            do_GET = reply
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", 0))); self.reply()
        http.server.HTTPServer(("127.0.0.1", 8001), H).serve_forever()
        ' "$variant"
      '';
    };

    services.model-router = {
      enable = true;
      polkit.units = ["llm-backend.service"];
      settings = {
        listen = "127.0.0.1:8080";
        defaultModel = "big";
        groups.gpu0 = {maxWait = "2m";};
        backends.llm = {
          group = "gpu0";
          upstream = "http://127.0.0.1:8001";
          driver = {
            kind = "systemd";
            systemd = {
              unit = "llm-backend.service";
              selectionFile = "/var/lib/model-router/variant";
              defaultVariant = "large";
              actionTimeout = "1m";
            };
          };
          readiness = {
            bodyContains = "loaded";
            timeout = "1m";
          };
        };
        models = {
          big = {
            backend = "llm";
            variant = "large";
          };
          small = {
            backend = "llm";
            variant = "small";
          };
        };
      };
    };
  };

  testScript = ''
    machine.wait_for_unit("llm-backend.service")
    machine.wait_for_unit("model-router.service")
    machine.wait_for_open_port(8080)
    machine.succeed("curl -sf http://127.0.0.1:8080/_router/healthz")
    machine.succeed("curl -sf http://127.0.0.1:8080/v1/models | grep -q '\"id\":\"small\"'")

    # A request for another variant restarts the backend via polkit.
    out = machine.succeed(
      "curl -sf -X POST -H 'Content-Type: application/json' "
      "-d '{\"model\":\"small\"}' http://127.0.0.1:8080/v1/chat/completions"
    )
    assert '"variant": "small"' in out, out
    machine.succeed("grep -qx small /var/lib/model-router/variant")

    # Hardening is in place.
    machine.succeed("systemctl show model-router -p NoNewPrivileges | grep -q yes")
    machine.succeed("systemctl show model-router -p User | grep -q model-router")
  '';
}
