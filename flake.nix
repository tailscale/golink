{
  description = "golink - A private shortlink service for tailnets";

  inputs = {
    nixpkgs.url = "nixpkgs/nixpkgs-unstable";
    parts.url = "github:hercules-ci/flake-parts";
    systems.url = "github:nix-systems/default";
  };

  outputs = inputs @ { self, parts, ... }: parts.lib.mkFlake { inherit inputs; } {
    systems = import inputs.systems;

    perSystem = { pkgs, ... }: {
      formatter = pkgs.nixpkgs-fmt;

      devShells.default = pkgs.mkShell { buildInputs = [ pkgs.go_1_23 ]; };

      packages.default =
        pkgs.buildGo123Module {
          pname = "golink";
          version =
            if (self ? shortRev)
            then self.shortRev
            else "dev";
          src = pkgs.nix-gitignore.gitignoreSource [ ] ./.;
          ldflags =
            let
              tsVersion = with builtins; head (match
                ".*tailscale.com v([0-9]+\.[0-9]+\.[0-9]+-[a-zA-Z]+).*"
                (readFile ./go.mod));
            in
            [
              "-w"
              "-s"
              "-X tailscale.com/version.longStamp=${tsVersion}"
              "-X tailscale.com/version.shortStamp=${tsVersion}"
            ];
          vendorHash = "sha256-myGEAOCJkeKGTzyLD6yBC10yHULxcbOnzseGVtYD7qM="; # SHA based on vendoring go.mod
        };
    };

    flake.overlays.default = final: prev: {
      golink = self.packages.${prev.system}.default;
    };

    flake.nixosModules.default = { config, lib, pkgs, ... }:
      let
        cfg = config.services.golink;
        inherit (lib)
          concatStringsSep
          escapeShellArg
          mkEnableOption
          mkIf
          mkOption
          optionalString
          optionals
          types
          ;
      in
      {
        options.services.golink = {
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

        config = mkIf cfg.enable {
          nixpkgs.overlays = [ self.overlays.default ];

          users.groups."${cfg.group}" = { };
          users.users."${cfg.user}" = {
            home = cfg.dataDir;
            createHome = true;
            group = "${cfg.group}";
            isSystemUser = true;
            isNormalUser = false;
            description = "user for golink service";
          };

          systemd.services.golink = {
            enable = true;
            script =
              let
                args = [ "--sqlitedb ${cfg.databaseFile}" ] ++ optionals cfg.verbose [ "--verbose" ];
              in
              ''
                ${optionalString (cfg.tailscaleAuthKeyFile != null) ''
                  export TS_AUTHKEY="$(head -n1 ${escapeShellArg cfg.tailscaleAuthKeyFile})"
                ''}

                ${cfg.package}/bin/golink ${concatStringsSep " " args}
              '';
            wantedBy = [ "multi-user.target" ];
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
