/**
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// No-op stub for dompurify. Mermaid imports this statically but
// vitepress-plugin-mermaid uses securityLevel:'loose' by default,
// so sanitization is effectively a passthrough. This avoids bundling
// the real dompurify (which also needs a DOM environment problematic
// for SSR). Safe because all diagram content is author-controlled.
const DOMPurify = {
  sanitize: (html) => html,
  addHook: () => {},
  removeHook: () => {},
  removeHooks: () => {},
  removeAllHooks: () => {},
  isSupported: true,
}

export default DOMPurify
