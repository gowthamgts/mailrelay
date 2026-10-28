// Copies README.md and docs/*.md into Starlight's content folder, so the
// Markdown in the repo stays the single source and still reads well on GitHub.
// It turns the first "# Title" into frontmatter, rewrites repo-relative links
// to site routes or GitHub URLs, and converts GitHub alerts into asides.

import { copyFileSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, posix } from 'node:path';
import { fileURLToPath } from 'node:url';

import { BASE, REPO_URL } from '../site.config.mjs';

const site = join(dirname(fileURLToPath(import.meta.url)), '..');
const root = join(site, '..');
const contentDir = join(site, 'src/content/docs');
const blobURL = `${REPO_URL}/blob/main/`;
const editURL = `${REPO_URL}/edit/main/`;

// docs/<name>.md -> site page. docs/README.md is the GitHub index; the sidebar replaces it.
const pages = readdirSync(join(root, 'docs'))
	.filter((f) => f.endsWith('.md') && f !== 'README.md')
	.map((f) => f.replace(/\.md$/, ''));

const asideTypes = { NOTE: 'note', TIP: 'tip', IMPORTANT: 'caution', WARNING: 'caution', CAUTION: 'danger' };

// rewriteTarget maps a link or image target, as written in fromDir (repo-relative),
// to where it lives on the site.
function rewriteTarget(target, fromDir) {
	if (/^([a-z]+:|#|\/)/i.test(target)) return target;
	const [path, hash] = target.split('#');
	const repoPath = posix.normalize(posix.join(fromDir, path));
	const anchor = hash ? `#${hash}` : '';

	if (repoPath === 'README.md') return `${BASE}/${anchor}`;
	const page = repoPath.match(/^docs\/([\w-]+)\.md$/)?.[1];
	if (page && pages.includes(page)) return `${BASE}/${page}/${anchor}`;
	if (repoPath.startsWith('docs/screenshots/')) return `${BASE}/${repoPath.slice('docs/'.length)}`;
	return blobURL + repoPath + anchor;
}

function rewriteLinks(md, fromDir) {
	return md
		.replace(/\]\(([^)\s]+)\)/g, (_, t) => `](${rewriteTarget(t, fromDir)})`)
		.replace(/\b(src|srcset)="([^"]+)"/g, (_, attr, t) => `${attr}="${rewriteTarget(t, fromDir)}"`);
}

// convertAlerts turns GitHub's "> [!NOTE]" blockquotes into Starlight asides.
function convertAlerts(md) {
	const out = [];
	const lines = md.split('\n');
	for (let i = 0; i < lines.length; i++) {
		const m = lines[i].match(/^> \[!(\w+)\]\s*$/);
		if (!m || !asideTypes[m[1]]) {
			out.push(lines[i]);
			continue;
		}
		out.push(`:::${asideTypes[m[1]]}`);
		while (i + 1 < lines.length && lines[i + 1].startsWith('>')) {
			out.push(lines[++i].replace(/^> ?/, ''));
		}
		out.push(':::');
	}
	return out.join('\n');
}

function frontmatter(fields) {
	const lines = Object.entries(fields).map(([k, v]) => `${k}: ${JSON.stringify(v)}`);
	return `---\n${lines.join('\n')}\n---\n\n`;
}

function writePage(slug, fields, body, fromDir) {
	const md = frontmatter(fields) + convertAlerts(rewriteLinks(body, fromDir)).trim() + '\n';
	writeFileSync(join(contentDir, `${slug}.md`), md);
}

rmSync(contentDir, { recursive: true, force: true });
mkdirSync(contentDir, { recursive: true });

for (const page of pages) {
	const src = readFileSync(join(root, 'docs', `${page}.md`), 'utf8');
	const title = src.match(/^# (.+)$/m)?.[1];
	if (!title) throw new Error(`docs/${page}.md has no "# Title" line`);
	const body = src.replace(/^# .+\n/m, '');
	writePage(page, { title, editUrl: `${editURL}docs/${page}.md` }, body, 'docs');
}

// The README becomes the home page, minus the logo and heading that the site
// header already shows, and the CI badges. Its centered tagline becomes the
// page description.
const readme = readFileSync(join(root, 'README.md'), 'utf8');
const tagline = readme.match(/<p align="center">([^<]+)<\/p>/)?.[1].trim();
const readmeBody = readme
	.replace(/^<p align="center">\s*<img src="assets\/logo\.png"[^]*?<\/p>\s*/, '')
	.replace(/^<h1 align="center">.*<\/h1>\s*/m, '')
	.replace(/<p align="center">[^<]+<\/p>\s*/, '')
	.replace(/<p align="center">(?:(?!<\/p>)[^])*badge(?:(?!<\/p>)[^])*<\/p>\s*/g, '');
writePage(
	'index',
	{ title: 'Introduction', description: tagline, editUrl: `${editURL}README.md` },
	`${tagline}\n\n${readmeBody}`,
	'',
);

// Static files referenced by the pages and the site config.
const publicDir = join(site, 'public');
const assetsDir = join(site, 'src/assets');
mkdirSync(join(publicDir, 'screenshots'), { recursive: true });
mkdirSync(assetsDir, { recursive: true });
for (const f of readdirSync(join(root, 'docs/screenshots'))) {
	copyFileSync(join(root, 'docs/screenshots', f), join(publicDir, 'screenshots', f));
}
copyFileSync(join(root, 'internal/webui/static/favicon.svg'), join(publicDir, 'favicon.svg'));
copyFileSync(join(root, 'assets/logo.png'), join(assetsDir, 'logo.png'));

console.log(`synced ${pages.length + 1} pages into src/content/docs`);
