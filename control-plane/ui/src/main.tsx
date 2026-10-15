import React from "react";
import ReactDOM from "react-dom/client";
import { setNonce } from "get-nonce";
import "@fontsource/ibm-plex-sans/latin-400.css";
import "@fontsource/ibm-plex-sans/latin-500.css";
import "@fontsource/ibm-plex-sans/latin-600.css";
import "@fontsource/ibm-plex-sans/latin-700.css";
import "@fontsource/ibm-plex-mono/latin-400.css";
import "@fontsource/ibm-plex-mono/latin-500.css";
import "@fontsource/ibm-plex-mono/latin-600.css";
import "@fontsource/ibm-plex-serif/latin-500.css";
import "./styles/app.css";
import App from "./App";

// The server puts a fresh per-request nonce in this meta tag and allows only
// that nonce for injected <style> elements (DES-HOR-459-04). Radix's scroll
// lock reads it through get-nonce.
const nonce = document
  .querySelector('meta[property="csp-nonce"]')
  ?.getAttribute("content");
if (nonce) setNonce(nonce);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
