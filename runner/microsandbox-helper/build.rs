fn main() {
    let schema = "../../contracts/microsandbox-helper/v1/helper.proto";
    println!("cargo:rerun-if-changed={schema}");
    prost_build::Config::new()
        .compile_protos(&[schema], &["../../contracts"])
        .expect("compile SecondBox Microsandbox helper protocol");
}
