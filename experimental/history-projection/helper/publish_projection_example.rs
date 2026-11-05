use std::io::Read;

#[tokio::main]
async fn main() {
    let mut manifest = String::new();
    let read = std::io::stdin().take(1024 * 1024 + 1).read_to_string(&mut manifest);
    let result = match read {
        Ok(_) if manifest.len() <= 1024 * 1024 => codex_thread_store::run_publish_projection(&manifest).await,
        Ok(_) => Err("manifest exceeds 1 MiB".to_string()),
        Err(error) => Err(error.to_string()),
    };
    match result {
        Ok(report) => println!("{report}"),
        Err(error) => { eprintln!("diagnostic publication failed: {error}"); std::process::exit(1); }
    }
}
