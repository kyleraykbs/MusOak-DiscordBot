# The NixOS module for the bot: one service, its own user, and the two secrets
# it needs read from files rather than from the store.
{ config, lib, pkgs, defaultPackage }:
let
  inherit (lib) mkEnableOption mkIf mkOption types;

  cfg = config.services.musoak-discord-bot;

  # The config the bot reads: everything but the secrets, which arrive in the
  # environment (MUSOAK_DISCORD_TOKEN, MUSOAK_DISCORD_PASSWORD) so that no
  # credential is ever written to the nix store.
  configFile = (pkgs.formats.json { }).generate "musoak-discord-bot.json" {
    token = "";
    serverURL = cfg.serverURL;
    guilds = cfg.guilds;
    account = {
      username = cfg.account.username;
      password = "";
      displayName = cfg.account.displayName;
    };
    # Both directories are systemd's: StateDirectory and CacheDirectory below
    # create them and give them to the service user.
    state = "/var/lib/musoak-discord-bot/state.db";
    cacheDir = "/var/cache/musoak-discord-bot";
    audioBufferMs = cfg.audioBufferMs;
    audioVolume = cfg.audioVolume;
    audioBitrateKbps = cfg.audioBitrateKbps;
  };

  start = pkgs.writeShellScript "musoak-discord-bot" ''
    set -eu
    export MUSOAK_DISCORD_TOKEN="$(cat ${cfg.tokenFile})"
    export MUSOAK_DISCORD_PASSWORD="$(cat ${cfg.account.passwordFile})"
    exec ${cfg.package}/bin/musoak-discord-bot --config ${configFile}
  '';
in
{
  options.services.musoak-discord-bot = {
    enable = mkEnableOption "the MusOak Discord bot";

    package = mkOption {
      type = types.package;
      default = defaultPackage;
      defaultText = lib.literalExpression "musoak-discord-bot";
      description = "The bot's package (it carries ffmpeg for the audio).";
    };

    tokenFile = mkOption {
      type = types.path;
      example = "/run/secrets/discord-token";
      description = ''
        File holding the Discord bot token, read by the service user. A clan
        variable's path works here.
      '';
    };

    serverURL = mkOption {
      type = types.str;
      example = "https://music.example.com";
      description = "The musoak server the bot talks to.";
    };

    guilds = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = ''
        Servers to register the commands in. Empty means every server the bot
        is in: they register the moment it is invited.
      '';
    };

    account = {
      username = mkOption {
        type = types.str;
        description = "The placeholder musoak account's username.";
      };
      passwordFile = mkOption {
        type = types.path;
        example = "/run/secrets/musoak-bot-password";
        description = "File holding that account's password, read by the service user.";
      };
      displayName = mkOption {
        type = types.str;
        default = "Discord Bot";
        description = "The name the room shows for the placeholder's queue items.";
      };
    };

    audioBufferMs = mkOption {
      type = types.ints.between 60 10000;
      default = 2000;
      description = ''
        Audio kept queued ahead of the voice connection. It absorbs hiccups and
        an inconsistent uplink; the bot plays that far behind the room, and its
        drift tolerance follows. Kept in step with the bot's own default.
      '';
    };

    audioVolume = mkOption {
      type = types.numbers.between 0.0 2.0;
      default = 0.125;
      description = ''
        Gain applied before encoding, as a fraction of each file's own level.
        The default is the bot's: music arrives mastered hot, and an eighth
        keeps a full-scale mix clear of the ceiling.
      '';
    };

    audioBitrateKbps = mkOption {
      type = types.ints.between 0 256;
      default = 0;
      description = "Encoder bitrate. Zero follows the voice channel's own setting.";
    };

    user = mkOption {
      type = types.str;
      default = "musoak-discord-bot";
      description = "The user the service runs as; the secret files must be readable by it.";
    };

    group = mkOption {
      type = types.str;
      default = "musoak-discord-bot";
      description = "The group the service runs as.";
    };

    extraEnvironment = mkOption {
      type = types.attrsOf types.str;
      default = { };
      example = { RUST_LOG = "info"; };
      description = "Extra environment variables for the service.";
    };
  };

  config = mkIf cfg.enable {
    users.users.${cfg.user} = {
      isSystemUser = true;
      group = cfg.group;
      home = "/var/lib/musoak-discord-bot";
      description = "MusOak Discord bot";
    };
    users.groups.${cfg.group} = { };

    systemd.services.musoak-discord-bot = {
      description = "MusOak Discord bot";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      serviceConfig = {
        ExecStart = start;
        User = cfg.user;
        Group = cfg.group;
        # The state database holds the linked accounts' credentials; systemd
        # makes both directories belong to the service user.
        StateDirectory = "musoak-discord-bot";
        CacheDirectory = "musoak-discord-bot";
        Environment = lib.mapAttrsToList (name: value: "${name}=${value}") cfg.extraEnvironment;
        Restart = "on-failure";
        RestartSec = 5;
      };
    };
  };
}
