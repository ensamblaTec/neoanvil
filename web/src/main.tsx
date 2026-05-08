import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './index.css';
import App from './App.tsx';

// SRE ERROR SNIFFER (Fricción Cero)
(function() {
    function sendErr(type: string, msg: string, stack: string) {
        fetch("http://127.0.0.1:8084/api/v1/log_frontend", {
            method: "POST",
            headers: {"Content-Type": "application/json"},
            body: JSON.stringify({type: type, message: msg, stack: stack || ""})
        }).catch(() => {}); // Silencio SRE si puente caído
    }

    window.addEventListener('error', function(e) {
        sendErr("Uncaught Error", e.message, e.error ? e.error.stack : "");
    });
    window.addEventListener('unhandledrejection', function(e) {
        sendErr("Promise Rejection", e.reason ? e.reason.toString() : "Unknown Rejection", "");
    });

    const origErr = console.error;
    console.error = function(...args) {
        sendErr("Console Error", args.map(a => String(a)).join(" "), "");
        origErr.apply(console, args);
    };

    const origWarn = console.warn;
    console.warn = function(...args) {
        sendErr("Console Warn", args.map(a => String(a)).join(" "), "");
        origWarn.apply(console, args);
    };

    const origLog = console.log;
    console.log = function(...args) {
        sendErr("Console Log", args.map(a => String(a)).join(" "), "");
        origLog.apply(console, args);
    };

    // Parchear WebSocket para interceptar errores asíncronos nativos
    const OrigWebSocket = window.WebSocket;
    window.WebSocket = function(url: string | URL, protocols?: string | string[]) {
        const ws = protocols ? new OrigWebSocket(url, protocols) : new OrigWebSocket(url);
        ws.addEventListener('error', function() {
            sendErr("WebSocket Error", `Connection to ${url} failed`, "");
        });
        return ws;
    } as unknown as typeof WebSocket;
})();

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);