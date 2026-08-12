# THIS FILE IS THE SOURCE OF TRUTH for mcavage/homebrew-tap's Formula/pix.rb.
#
# The `bump-tap` job in .github/workflows/publish.yml copies this file over the
# tap's copy on every release and then rewrites exactly three things: `version`,
# both `url`s, and both `sha256`s. Everything else -- notably `def install` --
# ships to users verbatim from here. Editing the tap directly will be silently
# overwritten by the next release; edit this file instead.
#
# WHY IT LIVES HERE. The tarball's contents and the formula's `install` block
# have to agree, and they used to live in two different repos with nothing
# comparing them. When `pix.1` was retired the tarball stopped shipping it, the
# formula kept running `man1.install "pix.1"`, and `brew install` died with
# ENOENT on the released v0.1.27 -- a break no test in either repo could see.
# tests/homebrew-formula.test.mjs now asserts this file only installs paths the
# publish workflow actually stages into the tarball, so that class of drift
# fails at review time instead of at a user's terminal.
#
# The version/url/sha256 values below are LAST RELEASE'S. They are placeholders
# that the bump job rewrites; do not hand-maintain them.
class Pix < Formula
  desc "Multi-model coding agent harness for Docker Sandboxes"
  homepage "https://github.com/mcavage/pix"
  # Required because Homebrew otherwise parses the archive suffix "arm64" as
  # version "64" and installs into Cellar/pix/64.
  version "0.1.28"
  license "MIT"

  livecheck do
    url :stable
    strategy :github_latest
  end

  # NO `depends_on` for sbx, deliberately. sbx ships as a CASK
  # (docker/homebrew-tap Casks/sbx.rb), and a Homebrew formula cannot depend on a
  # cask: the name would be resolved as a formula, not found, and every
  # `brew install mcavage/tap/pix` would fail. The caveats name the install
  # command instead, and `pix doctor` reports a missing sbx as a required gap
  # with the same command.
  on_macos do
    on_arm do
      url "https://github.com/mcavage/pix/releases/download/v0.1.28/pix_0.1.28_darwin_arm64.tar.gz"
      sha256 "3ef6fc3482e36f8dbc053d8449a522afcae637dc5b09d43ee408a9716d9e66e5"
    end
    on_intel do
      url "https://github.com/mcavage/pix/releases/download/v0.1.28/pix_0.1.28_darwin_amd64.tar.gz"
      sha256 "560a187cf9306143f47d9a8a4d54372611f68bce1fdee7001c0589637e3eb300"
    end
  end

  def install
    bin.install "pix", "pix-host"
    # The tarball carries the notices that legally have to travel with these
    # binaries: LICENSE for pix's own MIT s2, and NOTICE.md /
    # THIRD_PARTY_NOTICES.md / licenses/MPL-2.0.txt for the MPL-2.0
    # go-plugin/yamux code linked into pix-host (MPL-2.0 s3.1). install.sh
    # places the same four next to the binaries it installs; Homebrew must
    # not be the one channel that drops them.
    doc.install "LICENSE", "NOTICE.md", "THIRD_PARTY_NOTICES.md", "licenses"
  end

  def caveats
    <<~EOS
      Pix needs Docker Sandboxes, which is a cask and so is not installed
      automatically:

        brew install docker/tap/sbx

      Use that mainline cask, not docker/tap/sbx@nightly. The two conflict, so
      having one means uninstalling it before the other will install.

      Then run `pix setup` to finish onboarding.

      Before uninstalling this formula, run `pix state uninstall` FIRST.
      Then run `brew uninstall mcavage/tap/pix`. Reversing that order leaves
      launchd configured with a Cellar path that fails on its next launch.
    EOS
  end

  test do
    assert_equal version.to_s, shell_output("#{bin}/pix version").strip
    assert_equal version.to_s, shell_output("#{bin}/pix-host version").strip
  end
end
