use acadrust::{DwgReader, DxfWriter};
use std::env;
use std::fs;
use std::io::Read;
use std::process::ExitCode;

fn main() -> ExitCode {
    let mut args = env::args().skip(1);
    let Some(input) = args.next() else {
        eprintln!("usage: cad-dwg INPUT.dwg OUTPUT.dxf");
        return ExitCode::from(2);
    };
    let Some(output) = args.next() else {
        eprintln!("usage: cad-dwg INPUT.dwg OUTPUT.dxf");
        return ExitCode::from(2);
    };
    if args.next().is_some() {
        eprintln!("usage: cad-dwg INPUT.dwg OUTPUT.dxf");
        return ExitCode::from(2);
    }
    match convert(&input, &output) {
        Ok(()) => {
            verdict("ok", "");
            ExitCode::SUCCESS
        }
        Err(err) => {
            let _ = fs::remove_file(&output);
            verdict("unreadable", &err);
            ExitCode::from(1)
        }
    }
}

fn convert(input: &str, output: &str) -> Result<(), String> {
    let mut reader = DwgReader::from_file(input).map_err(|err| err.to_string())?;
    let outcome = reader.read_with_stats().map_err(|err| err.to_string())?;
    let mut writer = DxfWriter::new(&outcome.document);
    writer.binary = false;
    writer.write_to_file(output).map_err(|err| err.to_string())?;
    ascii_dxf(output)
}

fn ascii_dxf(path: &str) -> Result<(), String> {
    let mut file = fs::File::open(path).map_err(|err| err.to_string())?;
    let mut head = [0u8; 22];
    let n = file.read(&mut head).map_err(|err| err.to_string())?;
    if n == 0 {
        return Err("writer produced an empty dxf".into());
    }
    if head[..n].starts_with(b"AutoCAD Binary DXF") {
        return Err("writer produced a binary dxf".into());
    }
    Ok(())
}

fn verdict(verdict: &str, reason: &str) {
    println!(
        "CADVERDICT {{\"stage\":\"dwg\",\"verdict\":\"{verdict}\",\"reason\":{}}}",
        json_string(reason)
    );
}

fn json_string(value: &str) -> String {
    let mut out = String::from("\"");
    for c in value.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            c if c.is_control() => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}
