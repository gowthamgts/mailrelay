This program runs a SMTP server that receives emails, parse it and send them to a webhook. It also has a web UI (under internal/webui folder) to inspect the emails and the webhook responses.

If you're making any changes to frontend code, make sure to use @Browser agent to test the changes. Make sure there are no UI/UX issues. Assume the server is already running and the web UI is accessible at http://localhost:2623. If it's not up, you can start the server using `just dev` or `just dev-watch` in your terminal.

After making changes, always make sure to update README.md, config.example.yaml, and config.dev.yaml accordingly. Also, make sure to update the scripts in scripts/ folder to these new changes.

If you're taking screenshots, make sure to clean it up after using it.
