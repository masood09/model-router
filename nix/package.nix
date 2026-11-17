{
  lib,
  buildGoModule,
  version ? "dev",
  src,
}:
buildGoModule {
  pname = "model-router";
  inherit version src;

  # Standard library only: there is nothing to vendor.
  vendorHash = null;

  env.CGO_ENABLED = "0";
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  # The tests run as part of the build (checkPhase), with the race detector
  # off because CGO is disabled; CI runs them with -race separately.
  checkFlags = ["-count=1"];

  meta = {
    description = "OpenAI-compatible proxy that switches local LLM models on demand";
    homepage = "https://github.com/masood09/model-router";
    license = lib.licenses.mit;
    mainProgram = "model-router";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}
