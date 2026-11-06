# Changelog

## [0.2.0](https://github.com/Chrisgarlick/go-cms/compare/v0.1.2...v0.2.0) (2026-11-06)


### Features

* **admin:** list_columns and combinable field filters on content lists ([72f629e](https://github.com/Chrisgarlick/go-cms/commit/72f629ee0cea2354ab744f2bef8b613972898395))
* **admin:** lists sort by a date field named in sort.field ([d1492cb](https://github.com/Chrisgarlick/go-cms/commit/d1492cb1deb8a0546315849c22be7d3e3bc9b17d))
* **admin:** lists sort by a date field named in sort.field ([dcadcda](https://github.com/Chrisgarlick/go-cms/commit/dcadcda7dba3221b52a96d0689608846a36998cc))
* **admin:** the admin is built on kit ([01b6943](https://github.com/Chrisgarlick/go-cms/commit/01b6943d73f833a2535579e49361b11b8b9bfa63))
* **deploy:** several sites on one server behind nginx ([bcf7fae](https://github.com/Chrisgarlick/go-cms/commit/bcf7faea944cd28633645effbe835e90d4b44608))
* **fixtures:** featured images in content files (image, image_alt) ([e3a3269](https://github.com/Chrisgarlick/go-cms/commit/e3a3269486e757b9242bb7b41af451b5679881a6))
* **fixtures:** load every fixtures/*.yaml, content.yaml first ([3cf2570](https://github.com/Chrisgarlick/go-cms/commit/3cf2570bded364f5f78dc0ec0abb750b5bd460e0))
* **fixtures:** Markdown tables become table blocks; inline code in table cells ([feb70b9](https://github.com/Chrisgarlick/go-cms/commit/feb70b926037b8897bb77aa24c3293470d1e31a8))
* **fixtures:** rich-text fields may be given as Markdown in content files ([971170d](https://github.com/Chrisgarlick/go-cms/commit/971170d15d0cae76f0228f5f47cec50ad8fa6743))
* **import:** re-runnable content import with --update, --skip-field and --prune ([7ffad20](https://github.com/Chrisgarlick/go-cms/commit/7ffad205582f81fba15da33e0d4e78d4a1511aa0))
* **redirects:** match query strings (/blog?category={c} -&gt; /blog/category/{c}) ([b190532](https://github.com/Chrisgarlick/go-cms/commit/b190532daa642d9095d9ea13f6061903664b18c9))
* **render:** fromJSON template function for structured content in a code field ([e5f7084](https://github.com/Chrisgarlick/go-cms/commit/e5f70846cefc2cd7e593662d63a0e7f1ff6bb383))
* **site:** archive_title and archive_description on content types ([7aebf92](https://github.com/Chrisgarlick/go-cms/commit/7aebf926d67f5663a03e646bd1d8cfa13d438f0b))
* **site:** taxonomy title_format and description_format, per-term seo_title ([4a73ef3](https://github.com/Chrisgarlick/go-cms/commit/4a73ef32e5c57254f1d097bdd6e6d5bd25af0ead))
* **theme:** Tailwind build step (cms tailwind, cms dev, cms check) ([b457a7a](https://github.com/Chrisgarlick/go-cms/commit/b457a7ab36101e7bbc75ba004938be95b8eb9bde))


### Bug Fixes

* **config:** note why the credential path is safe to stat (gosec G703) ([bb8c4aa](https://github.com/Chrisgarlick/go-cms/commit/bb8c4aad892fde049438083ac0f736773397be01))
* **config:** under systemd, the LoadCredential copy of the key wins ([b7e11e5](https://github.com/Chrisgarlick/go-cms/commit/b7e11e554b64721a6cd1950f2cbeb657be6522ef))
* **deploy:** a site deploy links the idle colour to the release, not to the other colour ([9fcefb6](https://github.com/Chrisgarlick/go-cms/commit/9fcefb6aa548bf705e77a80e520021124faed5b6))
* **deploy:** enable the live colour and disable the old one after a switch ([bac6498](https://github.com/Chrisgarlick/go-cms/commit/bac6498ada62db18a8d40d9ef60e6279f799c1be))
* **deploy:** one-off commands get the key the way the services do ([e805b63](https://github.com/Chrisgarlick/go-cms/commit/e805b63950db3d4fbac1ed85f8a9548c47db9dac))
* **deploy:** rebuild the page cache after a switch ([f61ad5a](https://github.com/Chrisgarlick/go-cms/commit/f61ad5a1a8875e82ef55c58c1814984432a61d46))
* **deps:** brotli 1.2.7 for GO-2026-6888 ([e217127](https://github.com/Chrisgarlick/go-cms/commit/e217127ca0f0fc194bafbd9fded2e7677c5fa168))
* **deps:** brotli 1.2.7 for GO-2026-6888 ([c75070f](https://github.com/Chrisgarlick/go-cms/commit/c75070f32eccbaa4f7e5a24ac23723e210162773))
* **fixtures:** a bundle's fixture images are relative to fixtures/, like cms content import ([eb8dc26](https://github.com/Chrisgarlick/go-cms/commit/eb8dc26176aabbc73ec9feaf8f67ec59577b64a8))
* **redirects:** wrap the query-pattern parse error ([4da1aa7](https://github.com/Chrisgarlick/go-cms/commit/4da1aa7bed2d060468f4c969842dbd71ec0a4146))
* **render:** don't lose a re-render, build Tailwind before reloading, focusable code blocks ([e847684](https://github.com/Chrisgarlick/go-cms/commit/e8476849568286845e0ff29d4bca03936390e5a6))
* **theme:** the table block's scroll area is focusable (WCAG 2.1.1) ([7e0b7ba](https://github.com/Chrisgarlick/go-cms/commit/7e0b7ba6be1eb6a12bd288540045f74c8b3e6f4c))


### Performance Improvements

* **admin:** system fonts, one minified stylesheet, no kit.js ([ba6ca11](https://github.com/Chrisgarlick/go-cms/commit/ba6ca11d86405f0b52a2c5869bf2dcbfd3fa3468))

## [0.1.2](https://github.com/Chrisgarlick/go-cms/compare/v0.1.1...v0.1.2) (2026-10-26)


### Bug Fixes

* copy the per-platform binary into the Docker image ([e4d7325](https://github.com/Chrisgarlick/go-cms/commit/e4d73255040a7b496d2c425d5cbb23d231259dc7))
* copy the per-platform binary into the Docker image ([749efe5](https://github.com/Chrisgarlick/go-cms/commit/749efe56317f27c38d220f92a8ae24374ec11483))

## [0.1.1](https://github.com/Chrisgarlick/go-cms/compare/v0.1.0...v0.1.1) (2026-10-26)


### Bug Fixes

* lowercase the GHCR image name ([5ff3016](https://github.com/Chrisgarlick/go-cms/commit/5ff3016c87ff91bb1d7ff90b3fbdb2650fcbc796))

## 0.1.0 (2026-10-23)


### Bug Fixes

* build with Go 1.26.10 directly ([f14a299](https://github.com/Chrisgarlick/go-cms/commit/f14a2990038323030ebf476f564190d2e3da9df7))
