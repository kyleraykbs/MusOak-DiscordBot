{
  description = "MusOak Discord bot: a room per voice channel, songs queued by anyone";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    # The server the bot is a client of, and the musoakd the tests run their
    # contract against.
    musoak.url = "github:kyleraykbs/MusOak";
  };

  outputs = inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];

      perSystem = { pkgs, system, inputs', ... }:
        let
          inherit (pkgs) lib;

          # The binary, then wrapped with the ffmpeg it pipes songs through:
          # `nix run` needs no environment at all.
          unwrapped = pkgs.buildGoModule {
            pname = "musoak-discord-bot";
            version = "0.1.0";
            src = ./.;
            vendorHash = "sha256-Nf/JQeBxvsSf42t1HyYNbChjhv82X40f1pwsMhqVH8s=";
            # Pure Go: no cgo means no C toolchain, and the Discord gateway
            # behaves the same on every platform.
            env.CGO_ENABLED = 0;
            # The suites run against vendored modules, so the sandbox needs no
            # network. ffmpeg is what the voice tests transcode with, and the
            # link contract test spawns a real musoakd.
            nativeCheckInputs = [ pkgs.ffmpeg ];
            preCheck = ''
              export MUSOAKD=${inputs'.musoak.packages.musoakd}/bin/musoakd
            '';
            checkPhase = ''
              runHook preCheck
              go test ./...
              runHook postCheck
            '';
            meta = with lib; {
              description = "MusOak Discord bot: rooms per voice channels, songs for everyone";
              license = licenses.gpl2Only;
              mainProgram = "musoak-discord-bot";
            };
          };

          musoak-discord-bot = pkgs.runCommand "musoak-discord-bot-0.1.0"
            { nativeBuildInputs = [ pkgs.makeWrapper ]; } ''
            mkdir -p $out/bin
            cp ${unwrapped}/bin/musoak-discord-bot $out/bin/
            chmod +w $out/bin/musoak-discord-bot
            wrapProgram $out/bin/musoak-discord-bot \
              --prefix PATH : ${lib.makeBinPath [ pkgs.ffmpeg ]}
          '';
        in
        {
          packages = {
            default = musoak-discord-bot;
            inherit musoak-discord-bot;
          };

          apps.default = {
            type = "app";
            program = "${musoak-discord-bot}/bin/musoak-discord-bot";
          };

          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.gotools
              pkgs.ffmpeg
              # The tests spawn a real musoakd; in here it is on PATH under
              # the name they look for.
              inputs'.musoak.packages.musoakd
            ];
            shellHook = ''
              echo "musoak-discord-bot: go build ./... && go test ./..., then nix flake check"
            '';
          };

          checks = {
            # Building the package runs both suites (see checkPhase above):
            # checks.build is the wrapped result, checks.tests the same tests
            # under the name a reader expects.
            build = musoak-discord-bot;
            tests = unwrapped;
          };

          formatter = pkgs.nixfmt;
        };
    };
}
