#[tokio::main]
async fn main() {
    if let Err(error) = codex_thread_store::run_native_history_helper().await {
        eprintln!("native history maintenance failed: {error}");
        std::process::exit(1);
    }
}
