# Conformance Validator Test Data

The conformance validator currently does not require static fixtures, but the
CI image build copies each validator phase's `testdata` directory into the
image. Keep this directory tracked so the image build fails only when a phase's
fixture directory is genuinely missing.
