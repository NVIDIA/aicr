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

/**
 * Badge links for GitHub, License, PyPI, etc.
 * Uses a flex wrapper to display badges horizontally and hides Fern's
 * external-link icon that otherwise stacks under each badge image.
 * Requires the `.badge-links` CSS rule from main.css.
 */
export type BadgeItem = {
  href: string;
  src: string;
  alt: string;
};

export function BadgeLinks({ badges }: { badges: BadgeItem[] }) {
  return (
    <div
      className="badge-links"
      style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}
    >
      {badges.map((b) => (
        <a key={b.href} href={b.href} target="_blank" rel="noreferrer">
          <img src={b.src} alt={b.alt} />
        </a>
      ))}
    </div>
  );
}
