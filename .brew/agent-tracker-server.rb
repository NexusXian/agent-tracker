class AgentTrackerServer < Formula
  desc "Tmux-aware agent task tracker server"
  homepage "https://github.com/theniceboy/.config"
  # url, sha256 and version are filled in by scripts/install_brew_service.sh
  url "file:///tmp/tracker-server.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  version "local"

  def install
    bin.install "tracker-server"
  end

  service do
    run [opt_bin/"tracker-server"]
    keep_alive true
    working_dir var/"agent-tracker"
    environment_variables PATH: "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
    log_path var/"log/agent-tracker-server.log"
    error_log_path var/"log/agent-tracker-server.log"
  end
end
