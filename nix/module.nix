{
  config,
  lib,
  pkgs,
  utils,
  ...
}:
let
  cfg = config.services.regate;
  staticConfig =
    if cfg.staticConfigFile == null then pkgs.writeText "regate-static.hcl" "" else cfg.staticConfigFile;
in
{
  options.services.regate = {
    enable = lib.mkEnableOption "the Regate HTTP MCP gateway";
    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "Package providing the Regate executable and administration skills.";
    };
    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:8080";
      description = "HTTP listen address managed by the system service.";
    };
    staticConfigFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "HCL blocks owned by NixOS. These blocks are combined with dynamic HCL stored by Regate.";
    };
    bootstrapConfigFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Initial dynamic HCL loaded when Regate has no persisted configuration.";
    };
    environmentFiles = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Runtime paths to systemd environment files supplying REGATE_ADMIN_TOKEN and other credentials.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.environmentFiles != [ ];
        message = "services.regate.environmentFiles must supply REGATE_ADMIN_TOKEN.";
      }
    ];

    users.users.regate = {
      isSystemUser = true;
      group = "regate";
    };
    users.groups.regate = { };

    systemd.services.regate = {
      description = "Regate HTTP MCP gateway";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      environment.REGATE_SKILLS_DIR = "${cfg.package}/share/regate/skills";
      serviceConfig = {
        ExecStart = utils.escapeSystemdExecArgs (
          [
            (lib.getExe cfg.package)
            "-state" "/var/lib/regate"
            "-listen" cfg.listen
            "-wait-for-leader"
            "-static-config" "${staticConfig}"
          ]
          ++ lib.optionals (cfg.bootstrapConfigFile != null) [
            "-config" "${cfg.bootstrapConfigFile}"
          ]
        );
        EnvironmentFile = cfg.environmentFiles;
        StateDirectory = "regate";
        StateDirectoryMode = "0700";
        UMask = "0077";
        User = "regate";
        Group = "regate";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        Restart = "on-failure";
      };
    };
  };
}
