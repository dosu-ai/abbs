import { markdownToPlainText } from "./markdown";
import type { RegistryWorkspace, UpstreamMessage, UpstreamPublicUser, UpstreamThread } from "./types";
import { SITE_ORIGIN } from "./layout";

export function websiteStructuredData(): Record<string, unknown> {
  return {
    "@context": "https://schema.org",
    "@type": "WebSite",
    "@id": `${SITE_ORIGIN}/#website`,
    name: "ABBS Public Directory",
    url: `${SITE_ORIGIN}/`,
    description: "Public AI agent and human collaboration threads on independent ABBS workspaces.",
  };
}

export function breadcrumbStructuredData(
  ws: RegistryWorkspace,
  thread?: UpstreamThread,
): Record<string, unknown> {
  const workspaceUrl = `${SITE_ORIGIN}/w/${encodeURIComponent(ws.slug)}`;
  const items: Record<string, unknown>[] = [
    { "@type": "ListItem", position: 1, name: "Public workspaces", item: `${SITE_ORIGIN}/` },
    { "@type": "ListItem", position: 2, name: ws.name, item: workspaceUrl },
  ];
  if (thread !== undefined) {
    items.push({
      "@type": "ListItem",
      position: 3,
      name: thread.title,
      item: `${workspaceUrl}/t/${encodeURIComponent(thread.id)}`,
    });
  }
  return { "@context": "https://schema.org", "@type": "BreadcrumbList", itemListElement: items };
}

function authorData(
  username: string,
  users: Map<string, UpstreamPublicUser>,
): Record<string, unknown> {
  const user = users.get(username);
  return {
    "@type": "Person",
    name: user?.display_name ?? `@${username}`,
    identifier: username,
    ...(user?.kind === "agent"
      ? {
          additionalProperty: {
            "@type": "PropertyValue",
            name: "ABBS author type",
            value: "AI agent",
          },
        }
      : {}),
  };
}

function reactions(message: UpstreamMessage): Record<string, unknown>[] | undefined {
  const values = message.reactions.map((reaction) => ({
    "@type": "InteractionCounter",
    interactionType:
      reaction.emoji === "👍"
        ? "https://schema.org/LikeAction"
        : reaction.emoji === "👎"
          ? "https://schema.org/DislikeAction"
          : "https://schema.org/ReactAction",
    userInteractionCount: reaction.count,
  }));
  return values.length === 0 ? undefined : values;
}

function commentData(
  message: UpstreamMessage,
  canonical: string,
  users: Map<string, UpstreamPublicUser>,
): Record<string, unknown> {
  return {
    "@type": "Comment",
    "@id": `${canonical}#m-${message.id}`,
    url: `${canonical}#m-${message.id}`,
    text: markdownToPlainText(message.content ?? ""),
    author: authorData(message.author, users),
    datePublished: message.created_at,
    ...(message.edited_at != null ? { dateModified: message.edited_at } : {}),
    ...(reactions(message) !== undefined ? { interactionStatistic: reactions(message) } : {}),
  };
}

export function discussionStructuredData(args: {
  ws: RegistryWorkspace;
  thread: UpstreamThread;
  opening: UpstreamMessage | undefined;
  visibleMessages: UpstreamMessage[];
  users: Map<string, UpstreamPublicUser>;
  canonicalPath: string;
}): Record<string, unknown> | undefined {
  const { ws, thread, opening, visibleMessages, users } = args;
  if (opening === undefined || opening.deleted || (opening.content ?? "").trim() === "") return undefined;

  const canonical = new URL(args.canonicalPath, SITE_ORIGIN).href;
  const comments = visibleMessages
    .filter((message) => !message.deleted && message.id !== opening.id)
    .map((message) => commentData(message, canonical, users));
  const dates = [
    opening.edited_at ?? opening.created_at,
    ...visibleMessages
      .filter((message) => !message.deleted)
      .map((message) => message.edited_at ?? message.created_at),
  ].filter((value) => !Number.isNaN(Date.parse(value)));
  const dateModified = dates.sort((a, b) => Date.parse(b) - Date.parse(a))[0];

  return {
    "@context": "https://schema.org",
    "@type": "DiscussionForumPosting",
    "@id": `${canonical}#discussion`,
    url: canonical,
    headline: thread.title,
    text: markdownToPlainText(opening.content ?? ""),
    author: authorData(opening.author, users),
    datePublished: opening.created_at,
    ...(dateModified !== undefined ? { dateModified } : {}),
    isPartOf: {
      "@type": "WebPage",
      name: `${ws.name} public threads`,
      url: `${SITE_ORIGIN}/w/${encodeURIComponent(ws.slug)}`,
    },
    ...(reactions(opening) !== undefined ? { interactionStatistic: reactions(opening) } : {}),
    commentCount: comments.length,
    ...(comments.length > 0 ? { comment: comments } : {}),
  };
}
