import { mount } from 'svelte';
import './app.css';
import App from './App.svelte';

// `!` rather than a guard: #app is in index.html, so a null here means the
// document was replaced — a failure no fallback in this file could recover from,
// and portal/smoke asserts the mount in a real browser.
export default mount(App, { target: document.getElementById('app')! });
