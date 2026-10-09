# A Rust crate: format, lint, test, and a release build that waits for all
# three. Cargo's registry and target directory are kept between runs, which
# is most of what makes a Rust step that has to run tolerable.

RUST = "rust:1-slim-bookworm"
SOURCES = ["src", "tests", "Cargo.toml", "Cargo.lock"]

def cargo(name, command, needs = []):
    return step(
        name,
        image = RUST,
        run = command,
        inputs = SOURCES,
        needs = needs,
        caches = {"cargo-registry": "/usr/local/cargo/registry", "cargo-target": "/workspace/target"},
    )

def ci(ctx):
    fmt = cargo("fmt", "rustup component add rustfmt && cargo fmt --check")
    clippy = cargo("clippy", "rustup component add clippy && cargo clippy --all-targets -- -D warnings")
    test = cargo("test", "cargo test --locked")
    release = cargo("release build", "cargo build --release --locked", needs = [fmt, clippy, test])
    return [fmt, clippy, test, release]
