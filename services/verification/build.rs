fn main() {
    tonic_build::compile_protos("../../proto/verification.proto")
        .expect("failed to compile verification proto");
}
