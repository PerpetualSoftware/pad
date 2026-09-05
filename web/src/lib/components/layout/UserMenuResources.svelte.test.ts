// @vitest-environment jsdom
//
// TASK-2888: the Community link (the repo's GitHub Discussions) is a
// project-wide channel, so it must appear in BOTH the Cloud and the
// self-hosted Resources lists, and its href must come from the shared
// brand module rather than a re-typed literal.
import { describe, it, expect, afterEach } from "vitest";
import { render, cleanup } from "@testing-library/svelte";
import UserMenuResources from "./UserMenuResources.svelte";
import AuthFooter from "../auth/AuthFooter.svelte";
import { COMMUNITY_URL, GITHUB_REPO_URL } from "$lib/brand/links";

afterEach(() => cleanup());

function communityLink(container: HTMLElement): HTMLAnchorElement | null {
	return (
		Array.from(container.querySelectorAll("a")).find(
			(a) => a.textContent?.trim() === "Community",
		) ?? null
	);
}

describe("Community link (TASK-2888)", () => {
	it("points at the repo Discussions tab, derived from the repo URL", () => {
		expect(COMMUNITY_URL).toBe(`${GITHUB_REPO_URL}/discussions`);
	});

	it.each([true, false])(
		"user menu renders Community (cloudMode=%s), external",
		(cloudMode) => {
			const { container } = render(UserMenuResources, {
				props: { cloudMode },
			});
			const a = communityLink(container);
			expect(a, "Community link missing").not.toBeNull();
			expect(a?.getAttribute("href")).toBe(COMMUNITY_URL);
			expect(a?.getAttribute("target")).toBe("_blank");
			expect(a?.getAttribute("rel")).toBe("noopener noreferrer");
		},
	);

	it("user menu keeps GitHub before Community in both modes", () => {
		for (const cloudMode of [true, false]) {
			const { container } = render(UserMenuResources, {
				props: { cloudMode },
			});
			const labels = Array.from(container.querySelectorAll("a")).map(
				(a) => a.textContent?.trim(),
			);
			expect(labels.indexOf("GitHub")).toBeLessThan(
				labels.indexOf("Community"),
			);
			cleanup();
		}
	});

	it("auth footer renders Community on Cloud, right after GitHub", () => {
		const { container } = render(AuthFooter, {
			props: { cloudMode: true },
		});
		const labels = Array.from(container.querySelectorAll("a")).map((a) =>
			a.textContent?.trim(),
		);
		expect(labels.indexOf("Community")).toBe(labels.indexOf("GitHub") + 1);
		expect(communityLink(container)?.getAttribute("href")).toBe(
			COMMUNITY_URL,
		);
	});

	it("auth footer renders nothing on self-hosted (unchanged)", () => {
		const { container } = render(AuthFooter, {
			props: { cloudMode: false },
		});
		expect(container.querySelector("a")).toBeNull();
	});
});
