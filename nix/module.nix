# Generic NixOS module for model-router.
#
#   services.model-router = {
#     enable = true;
#     settings = { listen = "127.0.0.1:8922"; defaultModel = "..."; backends = { ... }; models = { ... }; };
#     polkit.units = [ "llm-backend.service" ];   # units the router may restart/stop
#   };
#
# `settings` is rendered verbatim to the JSON config file (see README.md for
# the schema); the config is validated with `-check-config` before start.
self: {
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.services.model-router;
  json = pkgs.formats.json {};
  configFile = json.generate "model-router.json" cfg.settings;
in {
  options = {
    services = {
      model-router = {
        enable = lib.mkEnableOption "model-router, an OpenAI-compatible proxy that switches local models";

        package = lib.mkOption {
          type = lib.types.package;
          default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
          defaultText = lib.literalExpression "model-router.packages.\${system}.default";
          description = "The model-router package.";
        };

        settings = lib.mkOption {
          type = json.type;
          default = {};
          description = "Configuration written to the JSON config file. See the README for the schema.";
        };

        user = lib.mkOption {
          type = lib.types.str;
          default = "model-router";
          description = "User the service runs as.";
        };

        group = lib.mkOption {
          type = lib.types.str;
          default = "model-router";
          description = "Group the service runs as.";
        };

        uid = lib.mkOption {
          type = lib.types.nullOr lib.types.int;
          default = null;
          description = "Fixed UID for the created user (null lets NixOS choose).";
        };

        gid = lib.mkOption {
          type = lib.types.nullOr lib.types.int;
          default = null;
          description = "Fixed GID for the created group (null lets NixOS choose).";
        };

        stateDirectoryMode = lib.mkOption {
          type = lib.types.str;
          default = "0755";
          description = ''
            Mode of /var/lib/model-router, where selection files usually live.
            The default lets a backend's own service user read them.
          '';
        };

        environmentFile = lib.mkOption {
          type = lib.types.nullOr lib.types.path;
          default = null;
          description = "Optional EnvironmentFile, e.g. for tokens referenced by auth.*TokensEnv.";
        };

        polkit = {
          units = lib.mkOption {
            type = lib.types.listOf lib.types.str;
            default = [];
            example = ["llm-backend.service"];
            description = "systemd units the router's user may manage through systemctl (systemd driver).";
          };
          verbs = lib.mkOption {
            type = lib.types.listOf (lib.types.enum ["start" "stop" "restart" "reload"]);
            default = ["restart" "stop"];
            description = "systemctl verbs allowed on those units.";
          };
        };
      };
    };
  };

  config = lib.mkIf cfg.enable {
    users = {
      users = lib.mkIf (cfg.user == "model-router") {
        model-router = {
          isSystemUser = true;
          inherit (cfg) group uid;
        };
      };
      groups = lib.mkIf (cfg.group == "model-router") {
        model-router = {
          inherit (cfg) gid;
        };
      };
    };

    security = lib.mkIf (cfg.polkit.units != []) {
      polkit = {
        enable = true;
        extraConfig = ''
          polkit.addRule(function(action, subject) {
            if (action.id == "org.freedesktop.systemd1.manage-units" &&
                subject.user == ${builtins.toJSON cfg.user} &&
                ${builtins.toJSON cfg.polkit.units}.indexOf(action.lookup("unit")) >= 0 &&
                ${builtins.toJSON cfg.polkit.verbs}.indexOf(action.lookup("verb")) >= 0) {
              return polkit.Result.YES;
            }
          });
        '';
      };
    };

    systemd = {
      services = {
        model-router = {
          description = "model-router: OpenAI-compatible model-switching proxy";
          wantedBy = ["multi-user.target"];
          after = ["network.target"];
          # The systemd driver calls systemctl from PATH by default.
          path = [config.systemd.package];
          restartTriggers = [configFile];
          serviceConfig = {
            ExecStartPre = "${lib.getExe cfg.package} -check-config -config ${configFile}";
            ExecStart = "${lib.getExe cfg.package} -config ${configFile}";
            User = cfg.user;
            Group = cfg.group;
            EnvironmentFile = lib.mkIf (cfg.environmentFile != null) cfg.environmentFile;
            StateDirectory = "model-router";
            StateDirectoryMode = cfg.stateDirectoryMode;
            Restart = "on-failure";
            RestartSec = 5;
            # Let in-flight requests finish (the router drains on SIGTERM).
            TimeoutStopSec = "90s";

            # Hardening. Unix sockets stay allowed so systemctl can reach systemd.
            NoNewPrivileges = true;
            CapabilityBoundingSet = "";
            AmbientCapabilities = "";
            ProtectSystem = "strict";
            ProtectHome = true;
            PrivateTmp = true;
            PrivateDevices = true;
            ProtectKernelTunables = true;
            ProtectKernelModules = true;
            ProtectKernelLogs = true;
            ProtectControlGroups = true;
            ProtectClock = true;
            ProtectHostname = true;
            ProtectProc = "invisible";
            ProcSubset = "pid";
            RestrictAddressFamilies = ["AF_INET" "AF_INET6" "AF_UNIX"];
            RestrictNamespaces = true;
            RestrictRealtime = true;
            RestrictSUIDSGID = true;
            LockPersonality = true;
            MemoryDenyWriteExecute = true;
            SystemCallArchitectures = "native";
            SystemCallFilter = ["@system-service" "~@privileged" "~@resources"];
            UMask = "0022";
          };
        };
      };
    };
  };
}
