{
  description = "golink - A private shortlink service for tailnets";

  inputs = {
    nixpkgs.url = "nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    { self
    , nixpkgs
    , flake-utils
    , ...
    }:
    let
      golinkVersion =
        if (self ? shortRev)
        then self.shortRev
        else "dev";
    in
    {
      overlay = final: prev:
        let
          pkgs = nixpkgs.legacyPackages.${prev.system};
        in
        rec {
          golink = pkgs.buildGo122Module rec {
            pname = "golink";
            version = golinkVersion;
            src = pkgs.nix-gitignore.gitignoreSource [ ] ./.;

            vendorHash = "sha256-DfHveQYngK/hPEspHL1JYQCc/m2CdBcy/8rqoR7eZec="; # SHA based on vendoring go.mod
          };
        };
    }
    // flake-utils.lib.eachDefaultSystem
      (system:
      let
        pkgs = import nixpkgs {
          overlays = [ self.overlay ];
          inherit system;
        };
      in
      rec {
        # `nix develop`
        devShell = pkgs.mkShell { buildInputs = [ pkgs.go_1_21 ]; };

        # `nix build`
        packages = with pkgs; {
          inherit golink;
        };

        defaultPackage = pkgs.golink;

        # `nix run`
        apps.golink = flake-utils.lib.mkApp {
          drv = packages.golink;
        };
        defaultApp = apps.golink;

        overlays.default = self.overlay;
      })
    // {
      nixosModules.default =
        { pkgs
        , lib
        , config
        , ...
        }:
        let
          cfg = config.services.golink;
        in
        {
          options = with lib; {
            services.golink = {
              enable = mkEnableOption "Enable golink";

              package = mkOption {
                type = types.package;
                description = ''
                  golink package to use
                '';
                default = pkgs.golink;
              };

              dataDir = mkOption {
                type = types.path;
                default = "/var/lib/golink";
                description = "Path to data dir";
              };

              user = mkOption {
                type = types.str;
                default = "golink";
                description = "User account under which golink runs.";
              };

              group = mkOption {
                type = types.str;
                default = "golink";
                description = "Group account under which golink runs.";
              };

              databaseFile = mkOption {
                type = types.path;
                default = "/var/lib/golink/golink.db";
                description = "Path to SQLite database";
              };

              tailscaleAuthKeyFile = mkOption {
                type = types.path;
                description = "Path to file containing the Tailscale Auth Key";
              };

              verbose = mkOption {
                type = types.bool;
                default = false;
              };
            };
          };
          config = lib.mkIf cfg.enable {
            users.users."${cfg.user}" = {
              home = cfg.dataDir;
              createHome = true;
              group = "${cfg.group}";
              isSystemUser = true;
              isNormalUser = false;
              description = "user for golink service";
            };
            users.groups."${cfg.group}" = { };

            systemd.services.golink = {
              enable = true;
              script =
                let
                  args =
                    [
                      "--sqlitedb ${cfg.databaseFile}"
                    ]
                    ++ lib.optionals cfg.verbose [ "--verbose" ];
                in
                ''
                  ${lib.optionalString (cfg.tailscaleAuthKeyFile != null) ''
                    export TS_AUTHKEY="$(head -n1 ${lib.escapeShellArg cfg.tailscaleAuthKeyFile})"
                  ''}

                  ${cfg.package}/bin/golink ${builtins.concatStringsSep " " args}
                '';
              wantedBy = [ "multi-user.target" ];
              wants = [ "network-online.target" ];
              after = [ "network-online.target" ];
              serviceConfig = {
                User = cfg.user;
                Group = cfg.group;
                Restart = "always";
                RestartSec = "15";
                WorkingDirectory = "${cfg.dataDir}";
              };
            };
          };
        };
    };
}
