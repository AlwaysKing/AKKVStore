(function () {
  "use strict";

  var editingCell = null;
  var editingType = "";
  var selectionMode = false;
  var selectedKeys = new Set();
  var pendingConfirmAction = null;

  function openDialog(dialog) {
    if (!dialog || dialog.open) {
      return;
    }
    if (typeof dialog.showModal === "function") {
      dialog.showModal();
    } else {
      dialog.setAttribute("open", "");
    }
    var input = dialog.querySelector("[autofocus]");
    if (input) {
      window.setTimeout(function () {
        input.focus();
        if (typeof input.select === "function") {
          input.select();
        }
      }, 0);
    }
  }

  function closeDialog(dialog) {
    if (!dialog) {
      return;
    }
    if (typeof dialog.close === "function") {
      dialog.close();
    } else {
      dialog.removeAttribute("open");
    }
  }

  function requestConfirmation(message, onConfirm) {
    var dialog = document.getElementById("confirm-dialog");
    if (!dialog) {
      return;
    }
    dialog.querySelector("[data-confirm-message]").textContent = message || "确定继续？";
    pendingConfirmAction = onConfirm;
    openDialog(dialog);
  }

  function resolveConfirmation(confirmed) {
    var dialog = document.getElementById("confirm-dialog");
    var action = pendingConfirmAction;
    pendingConfirmAction = null;
    if (dialog) {
      closeDialog(dialog);
    }
    if (confirmed && action) {
      action();
    }
  }

  function closeNewKeyEditor() {
    var editor = document.getElementById("new-key-editor-row");
    var trigger = document.getElementById("new-key-trigger-row");
    var form = document.getElementById("new-key-inline-form");
    if (form) {
      form.reset();
    }
    if (editor) {
      resetInlineValueEditor(editor.querySelector(".inline-value-editor"));
      editor.hidden = true;
    }
    if (trigger) {
      trigger.hidden = false;
    }
  }

  function openNewKeyEditor() {
    cancelCellEdit();
    var editor = document.getElementById("new-key-editor-row");
    var trigger = document.getElementById("new-key-trigger-row");
    if (!editor || !trigger) {
      return;
    }
    editor.hidden = false;
    trigger.hidden = true;
    var keyInput = editor.querySelector("[data-new-key-input]");
    if (keyInput) {
      window.setTimeout(function () {
        keyInput.focus();
      }, 0);
    }
  }

  function cancelCellEdit() {
    if (!editingCell) {
      return;
    }
    if (editingCell._originalHTML !== undefined) {
      editingCell.innerHTML = editingCell._originalHTML;
    }
    editingCell.classList.remove("editing", "loading");
    editingCell = null;
    editingType = "";
  }

  function submitForm(form) {
    if (!form) {
      return;
    }
    if (typeof form.requestSubmit === "function") {
      form.requestSubmit();
    } else {
      form.submit();
    }
  }

  function beginKeyEdit(cell) {
    cancelCellEdit();
    closeNewKeyEditor();
    editingCell = cell;
    editingType = "key";
    cell._originalHTML = cell.innerHTML;
    cell.classList.add("editing");

    var form = document.getElementById("inline-key-rename-form");
    form.setAttribute("action", cell.getAttribute("data-rename-url"));
    form.querySelector("[data-inline-old-key]").value = cell.getAttribute("data-key");

    var input = document.createElement("input");
    input.className = "inline-cell-editor";
    input.name = "new_key";
    input.value = cell.getAttribute("data-key");
    input.setAttribute("form", "inline-key-rename-form");
    input.title = "Enter 保存，Esc 取消";
    cell.replaceChildren(input);
    window.setTimeout(function () {
      input.focus();
      input.select();
    }, 0);
  }

  function beginContentTypeEdit(cell) {
    cancelCellEdit();
    closeNewKeyEditor();
    editingCell = cell;
    editingType = "content-type";
    cell._originalHTML = cell.innerHTML;
    cell.classList.add("editing");

    var form = document.getElementById("inline-content-type-edit-form");
    form.querySelector("[data-inline-content-type-key]").value = cell.getAttribute("data-key");

    var input = document.createElement("input");
    input.className = "inline-cell-editor";
    input.name = "content_type";
    input.value = cell.getAttribute("data-content-type") || "";
    input.setAttribute("form", "inline-content-type-edit-form");
    input.title = "Enter 保存，Esc 取消";
    cell.replaceChildren(input);
    window.setTimeout(function () {
      input.focus();
      input.select();
    }, 0);
  }

  function createInlineFileUpload(formID) {
    var label = document.createElement("label");
    label.className = "inline-file-button";
    label.appendChild(document.createTextNode("文件上传"));

    var input = document.createElement("input");
    input.className = "inline-file-input";
    input.type = "file";
    input.name = "file";
    input.setAttribute("form", formID);
    label.appendChild(input);
    return label;
  }

  function createInlineFileBadge() {
    var badge = document.createElement("span");
    badge.className = "inline-file-badge";
    badge.setAttribute("data-inline-file-badge", "");
    badge.hidden = true;

    var name = document.createElement("span");
    name.className = "inline-file-name";
    name.setAttribute("data-inline-file-name", "");
    badge.appendChild(name);

    var remove = document.createElement("button");
    remove.type = "button";
    remove.className = "inline-file-remove";
    remove.setAttribute("data-inline-file-remove", "");
    remove.setAttribute("aria-label", "删除文件");
    remove.textContent = "×";
    badge.appendChild(remove);
    return badge;
  }

  function setInlineFileMode(editor, fileName) {
    var textarea = editor.querySelector("textarea");
    var badge = editor.querySelector("[data-inline-file-badge]");
    if (!textarea || !badge) {
      return;
    }
    textarea.hidden = true;
    badge.hidden = false;
    editor.querySelector("[data-inline-file-name]").textContent = fileName || "文件";
  }

  function setInlineTextMode(editor, focus) {
    var textarea = editor.querySelector("textarea");
    var badge = editor.querySelector("[data-inline-file-badge]");
    if (!textarea || !badge) {
      return;
    }
    badge.hidden = true;
    textarea.hidden = false;
    if (focus) {
      textarea.focus();
    }
  }

  function resetInlineValueEditor(editor) {
    if (!editor) {
      return;
    }
    var name = editor.querySelector("[data-inline-file-name]");
    if (name) {
      name.textContent = "";
    }
    setInlineTextMode(editor, false);
  }

  function clearInlineFile(editor) {
    var input = editor.querySelector(".inline-file-input");
    if (input) {
      input.value = "";
      var form = document.getElementById(input.getAttribute("form"));
      var requireFile = form ? form.querySelector("[data-inline-require-file]") : null;
      if (requireFile) {
        requireFile.value = "";
        var contentType = form.querySelector("[data-inline-content-type]");
        var textContentType = editor.getAttribute("data-text-content-type");
        if (contentType && textContentType) {
          contentType.value = textContentType;
        }
      }
    }
    setInlineTextMode(editor, true);
  }

  function beginValueEdit(cell, data) {
    editingCell = cell;
    editingType = "value";
    cell.classList.remove("loading");
    cell.classList.add("editing");
    cell._originalHTML = cell.innerHTML;

    var form = document.getElementById("inline-value-edit-form");
    form.setAttribute("action", cell.getAttribute("data-edit-url"));
    form.querySelector("[data-inline-content-type]").value = data.contentType || "application/octet-stream";
    var originalIsFile = Boolean(data.external || !data.editable);
    form.querySelector("[data-inline-require-file]").value = originalIsFile ? "1" : "";

    var textarea = document.createElement("textarea");
    textarea.className = "inline-cell-editor";
    textarea.name = "value";
    textarea.rows = 1;
    textarea.value = data.value || "";
    textarea.setAttribute("form", "inline-value-edit-form");
    textarea.title = "Enter 保存，Shift+Enter 换行";

    var editor = document.createElement("div");
    editor.className = "inline-value-editor";
    editor.setAttribute("data-text-content-type",
      data.editable ? (data.contentType || "text/plain; charset=utf-8") : "text/plain; charset=utf-8");
    editor.appendChild(textarea);
    editor.appendChild(createInlineFileBadge());
    var upload = createInlineFileUpload("inline-value-edit-form");
    editor.appendChild(upload);
    cell.replaceChildren(editor);
    if (originalIsFile) {
      setInlineFileMode(editor, data.fileName || "当前文件");
    }
    window.setTimeout(function () {
      if (originalIsFile) {
        upload.querySelector("input").focus();
      } else {
        textarea.focus();
        textarea.setSelectionRange(textarea.value.length, textarea.value.length);
      }
    }, 0);
  }

  function startValueEdit(cell) {
    cancelCellEdit();
    closeNewKeyEditor();
    cell.classList.add("loading");
    fetch(cell.getAttribute("data-content-url"), {
      credentials: "same-origin",
      headers: { Accept: "application/json" }
    }).then(function (response) {
      if (!response.ok) {
        throw new Error("load failed");
      }
      return response.json();
    }).then(function (data) {
      beginValueEdit(cell, data);
    }).catch(function () {
      cell.classList.remove("loading");
      window.location.href = cell.getAttribute("data-edit-url");
    });
  }

  function startCellEdit(cell) {
    if (!cell || cell === editingCell) {
      return;
    }
    if (cell.getAttribute("data-cell") === "key") {
      beginKeyEdit(cell);
      return;
    }
    if (cell.getAttribute("data-cell") === "content-type") {
      beginContentTypeEdit(cell);
      return;
    }
    if (cell.getAttribute("data-cell") === "value") {
      startValueEdit(cell);
    }
  }

  function showToast(message, type) {
    if (!message) {
      return;
    }
    var stack = document.querySelector(".flash-stack");
    if (!stack) {
      stack = document.createElement("div");
      stack.className = "flash-stack";
      stack.setAttribute("aria-live", "polite");
      document.body.appendChild(stack);
    }
    var toast = document.createElement("div");
    toast.className = "flash " + (type || "ok");
    toast.textContent = message;
    stack.appendChild(toast);
    window.setTimeout(function () {
      toast.style.opacity = "0";
      toast.style.transform = "translateY(6px)";
      window.setTimeout(function () {
        toast.remove();
        if (stack.children.length === 0) {
          stack.remove();
        }
      }, 180);
    }, 5000);
  }

  function markInlineInputError(input) {
    if (!input) {
      return;
    }
    input.classList.add("inline-input-error");
    window.clearTimeout(input._errorTimer);
    input._errorTimer = window.setTimeout(function () {
      input.classList.remove("inline-input-error");
    }, 2600);
  }

  function showCopyFeedback(button) {
    if (!button) {
      return;
    }
    var copyIcon = button.querySelector(".copy-icon");
    var checkIcon = button.querySelector(".copy-check-icon");
    var label = button.querySelector("[data-copy-label]");
    var oldLabel = label ? label.textContent : "";
    var oldAria = button.getAttribute("aria-label");
    var oldTitle = button.getAttribute("title");
    window.clearTimeout(button._copyResetTimer);
    button.classList.add("copied");
    if (copyIcon) {
      copyIcon.hidden = true;
    }
    if (checkIcon) {
      checkIcon.hidden = false;
    }
    if (label) {
      label.textContent = "已复制";
    }
    button.setAttribute("aria-label", "已复制");
    button.title = "已复制";
    button._copyResetTimer = window.setTimeout(function () {
      button.classList.remove("copied");
      if (copyIcon) {
        copyIcon.hidden = false;
      }
      if (checkIcon) {
        checkIcon.hidden = true;
      }
      if (label) {
        label.textContent = oldLabel;
      }
      if (oldAria === null) {
        button.removeAttribute("aria-label");
      } else {
        button.setAttribute("aria-label", oldAria);
      }
      if (oldTitle === null) {
        button.removeAttribute("title");
      } else {
        button.setAttribute("title", oldTitle);
      }
    }, 1500);
  }

  function updateSelectionUI() {
    var panel = document.querySelector(".data-direct");
    if (!panel) {
      return;
    }
    panel.classList.toggle("selection-mode", selectionMode);
    var actions = panel.querySelector("[data-selection-actions]");
    if (actions) {
      actions.hidden = !selectionMode;
    }
    var toggle = panel.querySelector("[data-toggle-selection]");
    if (toggle) {
      toggle.textContent = selectionMode ? "退出选择" : "选择";
    }
    panel.querySelectorAll("[data-select-key]").forEach(function (checkbox) {
      var key = checkbox.getAttribute("data-select-key");
      var selected = selectedKeys.has(key);
      checkbox.checked = selected;
      var row = checkbox.closest("tr");
      if (row) {
        row.classList.toggle("selected", selected);
      }
    });
    var bulkDelete = panel.querySelector("[data-bulk-delete]");
    if (bulkDelete) {
      bulkDelete.disabled = selectedKeys.size === 0;
      bulkDelete.textContent = selectedKeys.size > 0
        ? "删除选中 (" + selectedKeys.size + ")"
        : "删除选中";
    }
  }

  function toggleSelectionMode() {
    selectionMode = !selectionMode;
    selectedKeys.clear();
    cancelCellEdit();
    closeNewKeyEditor();
    updateSelectionUI();
  }

  function submitBulkDelete() {
    if (selectedKeys.size === 0) {
      return;
    }
    requestConfirmation("确定删除选中的 " + selectedKeys.size + " 个 Key？", function () {
      var form = document.getElementById("bulk-delete-form");
      form.querySelectorAll('input[type="hidden"]').forEach(function (input) {
        input.remove();
      });
      selectedKeys.forEach(function (key) {
        var input = document.createElement("input");
        input.type = "hidden";
        input.name = "key";
        input.value = key;
        form.appendChild(input);
      });
      submitForm(form);
    });
  }

  function submitKeyInline(form, keyInput) {
    if (form.dataset.submitting === "1") {
      return;
    }
    if (keyInput) {
      keyInput.value = keyInput.value.trim();
    }
    form.dataset.submitting = "1";
    fetch(form.getAttribute("action"), {
      method: "POST",
      body: new FormData(form),
      credentials: "same-origin",
      headers: {
        Accept: "application/json",
        "X-Inline-Edit": "1"
      }
    }).then(function (response) {
      return response.json().catch(function () {
        return {};
      }).then(function (payload) {
        if (response.ok) {
          window.location.reload();
          return;
        }
        var message = payload.error || "保存失败，请重试";
        showToast(message, "err");
        markInlineInputError(keyInput);
        if (keyInput) {
          keyInput.focus();
          keyInput.select();
        }
      });
    }).catch(function () {
      showToast("网络异常，请重试", "err");
    }).then(function () {
      delete form.dataset.submitting;
    });
  }

  document.addEventListener("submit", function (event) {
    if (event.target.id === "new-key-inline-form") {
      event.preventDefault();
      submitKeyInline(event.target, document.querySelector("[data-new-key-input]"));
      return;
    }
    if (event.target.id === "inline-key-rename-form") {
      event.preventDefault();
      submitKeyInline(event.target, document.querySelector('input[form="inline-key-rename-form"][name="new_key"]'));
    }
  });

  document.addEventListener("input", function (event) {
    if (event.target.matches("[data-new-key-input], input[form='inline-key-rename-form']")) {
      event.target.classList.remove("inline-input-error");
      window.clearTimeout(event.target._errorTimer);
    }
  });

  document.addEventListener("click", function (event) {
    var tokenToggle = event.target.closest("[data-token-toggle]");
    if (tokenToggle) {
      var tokenBox = tokenToggle.closest("[data-token-secret]");
      var tokenDisplay = tokenBox ? tokenBox.querySelector("[data-token-display]") : null;
      var tokenValue = tokenBox ? tokenBox.getAttribute("data-token") : "";
      if (tokenDisplay && tokenValue) {
        var revealed = tokenDisplay.getAttribute("data-revealed") === "true";
        tokenDisplay.textContent = revealed ? "*".repeat(tokenValue.length) : tokenValue;
        tokenDisplay.setAttribute("data-revealed", revealed ? "false" : "true");
        tokenToggle.setAttribute("aria-label", revealed ? "显示 Token" : "隐藏 Token");
        tokenToggle.title = revealed ? "显示 Token" : "隐藏 Token";
      }
      return;
    }

    var tokenCopy = event.target.closest("[data-token-copy]");
    if (tokenCopy) {
      var copyBox = tokenCopy.closest("[data-token-secret]");
      var copyValue = copyBox ? copyBox.getAttribute("data-token") : "";
      if (!copyValue) {
        return;
      }
      if (!navigator.clipboard) {
        showToast("当前环境无法访问剪贴板", "err");
        return;
      }
      navigator.clipboard.writeText(copyValue).then(function () {
        showCopyFeedback(tokenCopy);
      }).catch(function () {
        showToast("复制 Token 失败", "err");
      });
      return;
    }

    var cancelNewKeyTarget = event.target.closest("[data-cancel-new-key]");
    if (cancelNewKeyTarget) {
      closeNewKeyEditor();
      return;
    }

    var selectionToggle = event.target.closest("[data-toggle-selection]");
    if (selectionToggle) {
      toggleSelectionMode();
      return;
    }

    var bulkDeleteTarget = event.target.closest("[data-bulk-delete]");
    if (bulkDeleteTarget) {
      submitBulkDelete();
      return;
    }

    var removeFileTarget = event.target.closest("[data-inline-file-remove]");
    if (removeFileTarget) {
      var valueEditor = removeFileTarget.closest(".inline-value-editor");
      if (valueEditor) {
        clearInlineFile(valueEditor);
      }
      return;
    }

    var cell = event.target.closest("td.editable-cell");
    if (cell) {
      if (selectionMode) {
        return;
      }
      if (!event.target.closest("input, textarea, button, select")) {
        startCellEdit(cell);
      }
      return;
    }

    var newKeyTarget = event.target.closest("[data-new-key]");
    if (newKeyTarget) {
      openNewKeyEditor();
      return;
    }

    var openTarget = event.target.closest("[data-open-dialog]");
    if (openTarget) {
      openDialog(document.getElementById(openTarget.getAttribute("data-open-dialog")));
      return;
    }

    var closeTarget = event.target.closest("[data-close-dialog]");
    if (closeTarget) {
      closeDialog(closeTarget.closest("dialog"));
      return;
    }

    if (event.target.tagName === "DIALOG") {
      closeDialog(event.target);
      return;
    }

    var confirmTarget = event.target.closest("[data-confirm]");
    if (confirmTarget) {
      var message = confirmTarget.getAttribute("data-confirm") || "确定继续？";
      event.preventDefault();
      requestConfirmation(message, function () {
        if (confirmTarget.form) {
          confirmTarget.form.requestSubmit(confirmTarget);
        }
      });
      return;
    }

    var copyTarget = event.target.closest("[data-copy]");
    if (copyTarget) {
      var id = copyTarget.getAttribute("data-copy");
      var el = id ? document.getElementById(id) : null;
      if (!el) {
        return;
      }
      var text = (el.textContent || "").trim();
      if (!navigator.clipboard) {
        window.prompt("复制下面的内容：", text);
        return;
      }
      navigator.clipboard.writeText(text).then(function () {
        showCopyFeedback(copyTarget);
      }).catch(function () {
        window.prompt("复制下面的内容：", text);
      });
    }
  });

  document.addEventListener("change", function (event) {
    if (event.target.matches("[data-select-key]")) {
      if (!selectionMode) {
        return;
      }
      var selectedKey = event.target.getAttribute("data-select-key");
      if (event.target.checked) {
        selectedKeys.add(selectedKey);
      } else {
        selectedKeys.delete(selectedKey);
      }
      updateSelectionUI();
      return;
    }
    if (!event.target.matches(".inline-file-input") || !event.target.files.length) {
      return;
    }
    var editor = event.target.closest(".inline-value-editor");
    if (editor) {
      setInlineFileMode(editor, event.target.files[0].name);
    }
  });

  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape") {
      if (editingCell) {
        cancelCellEdit();
        return;
      }
      if (selectionMode) {
        selectionMode = false;
        selectedKeys.clear();
        updateSelectionUI();
        return;
      }
      var newKeyEditor = document.getElementById("new-key-editor-row");
      if (newKeyEditor && !newKeyEditor.hidden) {
        closeNewKeyEditor();
      }
      return;
    }
    if (event.key !== "Enter" || event.shiftKey) {
      return;
    }

    if (editingCell && editingCell.contains(event.target)) {
      event.preventDefault();
      if (editingType === "key") {
        submitForm(document.getElementById("inline-key-rename-form"));
      } else if (editingType === "content-type") {
        submitForm(document.getElementById("inline-content-type-edit-form"));
      } else if (editingType === "value") {
        submitForm(document.getElementById("inline-value-edit-form"));
      }
      return;
    }
    var editor = document.getElementById("new-key-editor-row");
    if (editor && !editor.hidden && editor.contains(event.target)) {
      event.preventDefault();
      submitForm(document.getElementById("new-key-inline-form"));
    }
  });

  var confirmDialog = document.getElementById("confirm-dialog");
  if (confirmDialog) {
    confirmDialog.querySelectorAll("[data-confirm-cancel]").forEach(function (button) {
      button.addEventListener("click", function () {
        resolveConfirmation(false);
      });
    });
    var confirmAccept = confirmDialog.querySelector("[data-confirm-accept]");
    if (confirmAccept) {
      confirmAccept.addEventListener("click", function () {
        resolveConfirmation(true);
      });
    }
    confirmDialog.addEventListener("close", function () {
      pendingConfirmAction = null;
    });
  }

  document.querySelectorAll("[data-api-path]").forEach(function (element) {
    var apiPath = element.getAttribute("data-api-path");
    if (apiPath) {
      element.textContent = new URL(apiPath, window.location.origin).href;
    }
  });

  var loginTabs = document.querySelectorAll("[data-login-tab]");
  if (loginTabs.length) {
    var activateLoginTab = function (method, focus) {
      document.querySelectorAll("[data-login-tab]").forEach(function (tab) {
        var active = tab.getAttribute("data-login-tab") === method;
        tab.classList.toggle("active", active);
        tab.setAttribute("aria-selected", active ? "true" : "false");
      });
      document.querySelectorAll("[data-login-panel]").forEach(function (panel) {
        panel.hidden = panel.getAttribute("data-login-panel") !== method;
      });
      if (focus) {
        var panel = document.querySelector('[data-login-panel="' + method + '"]');
        var input = panel ? panel.querySelector("input:not([type='hidden'])") : null;
        if (input) {
          input.focus();
        }
      }
    };
    loginTabs.forEach(function (tab) {
      tab.addEventListener("click", function () {
        activateLoginTab(tab.getAttribute("data-login-tab"), true);
      });
    });
  }

  var loginSendCodeButton = document.querySelector("[data-login-send-code]");
  var loginSendError = document.querySelector("[data-login-send-error]");
  if (loginSendCodeButton) {
    var loginCodeTimer = null;
    var startLoginCodeCountdown = function (seconds) {
      var remaining = Math.max(0, Number(seconds) || 0);
      window.clearInterval(loginCodeTimer);
      var update = function () {
        if (remaining > 0) {
          loginSendCodeButton.disabled = true;
          loginSendCodeButton.textContent = remaining + "s";
          remaining--;
          return;
        }
        window.clearInterval(loginCodeTimer);
        loginCodeTimer = null;
        loginSendCodeButton.disabled = false;
        loginSendCodeButton.textContent = "发送验证码";
      };
      update();
      if (remaining >= 0) {
        loginCodeTimer = window.setInterval(update, 1000);
      }
    };

    loginSendCodeButton.addEventListener("click", function (event) {
      event.preventDefault();
      if (loginSendCodeButton.disabled) {
        return;
      }
      setInlineError(loginSendError, "");
      loginSendCodeButton.disabled = true;
      loginSendCodeButton.textContent = "发送中...";
      fetch("/admin/login/code", {
        method: "POST",
        credentials: "same-origin",
        headers: {
          Accept: "application/json",
          "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8"
        }
      }).then(function (response) {
        if (response.redirected) {
          throw new Error("登录状态已失效，请刷新页面");
        }
        return response.json().catch(function () {
          return {};
        }).then(function (payload) {
          if (!response.ok) {
            if (payload.retry_after > 0) {
              startLoginCodeCountdown(payload.retry_after);
            }
            throw new Error(payload.error || "发送验证码失败");
          }
          startLoginCodeCountdown(payload.retry_after || 60);
        });
      }).catch(function (error) {
        if (!loginSendCodeButton.disabled || loginSendCodeButton.textContent === "发送中...") {
          loginSendCodeButton.disabled = false;
          loginSendCodeButton.textContent = "发送验证码";
        }
        setInlineError(loginSendError, error.message || "发送验证码失败");
      });
    });
  }

  var passwordAuthToggle = document.querySelector('[data-auth-toggle="password"]');
  var barkAuthToggle = document.querySelector('[data-auth-toggle="bark"]');
  var passwordAuthBody = document.querySelector('[data-auth-body="password"]');
  var barkAddressInput = document.querySelector("[data-bark-address]");
  var barkVerifyButton = document.querySelector("[data-bark-verify]");
  var barkVerifyError = document.querySelector("[data-bark-verify-error]");
  var barkVerifiedHint = document.querySelector("[data-bark-verified-hint]");
  var barkUnverifiedHint = document.querySelector("[data-bark-unverified-hint]");
  var barkVerifyDialog = document.getElementById("bark-verify-dialog");
  var barkSavedAddress = barkAddressInput ? barkAddressInput.getAttribute("data-bark-saved-address") || "" : "";

  function setInlineError(element, message) {
    if (!element) {
      return;
    }
    element.textContent = message || "";
    element.hidden = !message;
  }

  function postAdminAction(url, values) {
    return fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8"
      },
      body: values.toString()
    }).then(function (response) {
      if (response.redirected) {
        throw new Error("登录状态已失效，请重新登录");
      }
      return response.json().catch(function () {
        return {};
      }).then(function (payload) {
        if (!response.ok) {
          throw new Error(payload.error || "操作失败，请重试");
        }
        return payload;
      });
    });
  }

  function barkAddressVerified() {
    if (!barkAddressInput || !barkSavedAddress) {
      return false;
    }
    return barkAddressInput.value.trim() === barkSavedAddress;
  }

  function syncAuthMethodUI() {
    if (passwordAuthToggle && barkAuthToggle) {
      var passwordEnabled = passwordAuthToggle.checked;
      var barkEnabled = barkAuthToggle.checked;
      var verified = barkAddressVerified();
      if (passwordAuthBody) {
        passwordAuthBody.hidden = !passwordEnabled;
      }
      passwordAuthToggle.disabled = passwordEnabled && !barkEnabled;
      barkAuthToggle.disabled = (barkEnabled && !passwordEnabled) || (!barkEnabled && !verified);
      if (barkVerifiedHint) {
        barkVerifiedHint.hidden = !verified;
      }
      if (barkUnverifiedHint) {
        barkUnverifiedHint.hidden = verified;
      }
      if (barkVerifyButton) {
        barkVerifyButton.textContent = verified ? "重新验证" : "保存并验证";
      }
    }
  }

  document.querySelectorAll("[data-auth-toggle]").forEach(function (toggle) {
    toggle.addEventListener("change", function () {
      var method = toggle.getAttribute("data-auth-toggle");
      var enabled = toggle.checked;
      var values = new URLSearchParams({
        method: method,
        enabled: enabled ? "1" : "0"
      });
      toggle.disabled = true;
      postAdminAction("/admin/system/auth/method", values)
        .then(function () {
          showToast(method === "password"
            ? (enabled ? "密码认证已启用" : "密码认证已关闭")
            : (enabled ? "Bark 推送认证已启用" : "Bark 推送认证已关闭"), "ok");
          syncAuthMethodUI();
        })
        .catch(function (error) {
          toggle.checked = !enabled;
          showToast(error.message || "更新认证方式失败", "err");
          syncAuthMethodUI();
        });
    });
  });

  if (barkAddressInput) {
    barkAddressInput.addEventListener("input", function () {
      setInlineError(barkVerifyError, "");
      syncAuthMethodUI();
    });
    syncAuthMethodUI();
  }

  if (barkVerifyButton && barkAddressInput && barkVerifyDialog) {
    var barkCodeStep = barkVerifyDialog.querySelector("[data-bark-code-step]");
    var barkSuccessStep = barkVerifyDialog.querySelector("[data-bark-success-step]");
    var barkVerifyAddress = barkVerifyDialog.querySelector("[data-bark-verify-address]");
    var barkCodeForm = barkVerifyDialog.querySelector("[data-bark-code-form]");
    var barkCodeInput = barkVerifyDialog.querySelector("[data-bark-code-input]");
    var barkCodeError = barkVerifyDialog.querySelector("[data-bark-code-error]");
    var barkCodeSubmit = barkVerifyDialog.querySelector("[data-bark-code-submit]");
    var barkCodeSubmitText = barkCodeSubmit ? barkCodeSubmit.textContent : "";

    barkVerifyButton.addEventListener("click", function () {
      var address = barkAddressInput.value.trim();
      setInlineError(barkVerifyError, "");
      if (!address) {
        setInlineError(barkVerifyError, "请填写 Bark 地址");
        barkAddressInput.focus();
        return;
      }
      if (!barkAddressInput.checkValidity()) {
        barkAddressInput.reportValidity();
        return;
      }

      barkVerifyButton.disabled = true;
      barkVerifyButton.textContent = "发送中...";
      var values = new URLSearchParams({ bark_address: address });
      postAdminAction("/admin/system/auth/bark/send", values)
        .then(function () {
          barkCodeStep.hidden = false;
          barkSuccessStep.hidden = true;
          barkVerifyAddress.textContent = address;
          barkCodeInput.value = "";
          setInlineError(barkCodeError, "");
          openDialog(barkVerifyDialog);
        })
        .catch(function (error) {
          setInlineError(barkVerifyError, error.message || "发送验证码失败");
        })
        .finally(function () {
          barkVerifyButton.disabled = false;
          syncAuthMethodUI();
        });
    });

    barkCodeForm.addEventListener("submit", function (event) {
      event.preventDefault();
      var code = barkCodeInput.value.trim();
      setInlineError(barkCodeError, "");
      if (!/^[0-9]{4}$/.test(code)) {
        setInlineError(barkCodeError, "请输入 4 位数字验证码");
        barkCodeInput.focus();
        return;
      }

      var values = new URLSearchParams({
        bark_address: barkAddressInput.value.trim()
      });
      values.set("bark_code", code);
      barkCodeSubmit.disabled = true;
      barkCodeSubmit.textContent = "验证中...";
      postAdminAction("/admin/system/auth/bark/verify", values)
        .then(function (payload) {
          if (payload.address) {
            barkAddressInput.value = payload.address;
            barkSavedAddress = payload.address;
            barkAddressInput.setAttribute("data-bark-saved-address", payload.address);
          }
          syncAuthMethodUI();
          barkCodeStep.hidden = true;
          barkSuccessStep.hidden = false;
          var finishButton = barkSuccessStep.querySelector("button");
          if (finishButton) {
            finishButton.focus();
          }
        })
        .catch(function (error) {
          setInlineError(barkCodeError, error.message || "验证失败，请重试");
          barkCodeInput.focus();
          barkCodeInput.select();
        })
        .finally(function () {
          barkCodeSubmit.disabled = false;
          barkCodeSubmit.textContent = barkCodeSubmitText;
        });
    });

    barkVerifyDialog.addEventListener("close", function () {
      barkCodeStep.hidden = false;
      barkSuccessStep.hidden = true;
      barkCodeInput.value = "";
      setInlineError(barkCodeError, "");
    });
    syncAuthMethodUI();
  }

  window.setTimeout(function () {
    document.querySelectorAll(".flash-stack .flash.ok").forEach(function (flash) {
      if (flash.querySelector(".token-box")) {
        return;
      }
      flash.style.opacity = "0";
      flash.style.transform = "translateY(6px)";
      window.setTimeout(function () {
        var stack = flash.parentElement;
        flash.remove();
        if (stack && stack.children.length === 0) {
          stack.remove();
        }
      }, 180);
    });
  }, 3200);

  document.querySelectorAll('dialog[data-auto-open="true"]').forEach(openDialog);
})();
