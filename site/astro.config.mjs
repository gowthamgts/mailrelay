import starlight from '@astrojs/starlight';
import { defineConfig } from 'astro/config';

import { BASE, REPO_URL, SITE } from './site.config.mjs';

export default defineConfig({
	site: SITE,
	base: BASE,
	integrations: [
		starlight({
			title: 'mailrelay',
			description: 'Turn incoming email into webhooks.',
			logo: { src: './src/assets/logo.png' },
			favicon: '/favicon.svg',
			social: [{ icon: 'github', label: 'GitHub', href: REPO_URL }],
			sidebar: [
				{ label: 'Introduction', link: '/' },
				{
					label: 'Guides',
					items: ['self-hosting', 'reverse-proxy', 'webhooks', 'troubleshooting'],
				},
				{ label: 'Reference', items: ['prometheus'] },
			],
		}),
	],
});
