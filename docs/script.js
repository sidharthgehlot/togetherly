(function () {
  var copyButton = document.querySelector("[data-copy-invite]");
  var copyStatus = document.querySelector("[data-copy-status]");

  if (!copyButton || !copyStatus) {
    return;
  }

  var inviteText = [
    "Watch with me on togetherly:",
    "1. Download the app: https://github.com/sidharthgehlot/togetherly/releases/latest/download/togetherly.exe",
    "2. Open the same video in VLC.",
    "3. Join my room code in togetherly."
  ].join("\n");

  function setStatus(message) {
    copyStatus.textContent = message;
    window.clearTimeout(setStatus.timer);
    setStatus.timer = window.setTimeout(function () {
      copyStatus.textContent = "";
    }, 3000);
  }

  copyButton.addEventListener("click", function () {
    if (!navigator.clipboard) {
      setStatus("Copy is not available in this browser.");
      return;
    }

    navigator.clipboard
      .writeText(inviteText)
      .then(function () {
        setStatus("Invite text copied.");
      })
      .catch(function () {
        setStatus("Could not copy invite text.");
      });
  });
})();
