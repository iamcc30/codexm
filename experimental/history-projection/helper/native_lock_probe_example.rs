#[path = "native_lock_probe_support/mod.rs"]
mod native_lock_probe;

fn main() {
    let code = match native_lock_probe::run(std::io::BufReader::new(std::io::stdin())) {
        Ok(code) => code,
        Err(error) => {
            eprintln!("synthetic native lock diagnostic failed: {error}");
            1
        }
    };
    std::process::exit(code);
}
