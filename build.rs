fn main() {
    println!("cargo:rerun-if-changed=assets/icons/amux.ico");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows") {
        winresource::WindowsResource::new()
            .set_icon("assets/icons/amux.ico")
            .compile()
            .expect("compile native amux icon");
    }
}
