// Minimal stand-in for pi's interactive-mode.js addMessageToChat user branch.
// Only the anchor the patch rewrites, plus enough around it to be runnable so a
// test can exercise the injected stripping rather than just grep for it.
export class InteractiveMode {
    constructor(text) {
        this.text = text;
        this.rendered = null;
        this.history = [];
    }
    getUserMessageText(_message) {
        return this.text;
    }
    addMessageToChat(message, options) {
        switch (message.role) {
            case "user": {
                const textContent = this.readUserText(message); // upstream renamed the getter
                if (textContent) {
                    this.rendered = textContent;
                    if (options?.populateHistory) {
                        this.history.push(textContent);
                    }
                }
                break;
            }
        }
    }
}
