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

      overlay = final: prev:
        let
          pkgs = nixpkgs.legacyPackages.${prev.system};
        in
        {
          golink = pkgs.buildGo122Module {
            pname = "golink";
            version = golinkVersion;
            src = pkgs.nix-gitignore.gitignoreSource [ ] ./.;

            vendorHash = "sha256-fKa24Rnd2Kc/qPSMEyuojkq2oHGrVVpYfypUFrvvztQ="; # SHA based on vendoring go.mod
          };
        };
    in
    flake-utils.lib.eachDefaultSystem
      (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          overlays = [ overlay ];
        };
      in
      rec {
        # `nix develop`
        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go_1_22
            pkgs.nodejs-slim
            pkgs.yarn
          ];
          buildInputs = [ pkgs.go_1_22 ];
        };

        # `nix build`
        packages = with pkgs; {
          inherit golink;
          default = golink;
        };

        # `nix run`
        apps.golink = flake-utils.lib.mkApp {
          drv = packages.golink;
        };
        apps.default = apps.golink;

        overlays = overlay;
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
