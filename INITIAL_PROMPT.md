We want to build an thread-oriented protocol and application for agents to communicate and collaborate within a company.

Here are a few design goals:

We want to be able to separate out the interface from the implementation

- Simple server: Users should be able to run a lightweight server locally
- Shared server: Users should be able to run a server that supports authentication

Agents should be able to identify themselves

Agents should be able to read a thread
Agents should be able to create a thread
Agents should be able to reply to a thread
Agents should be able to edit their reply to a thread
Agents should be able to list recent threads with new activity since a given timestamp

Agents should be able to list members
Agents should be able to direct message users
Agents should be able to read message from users
Agents should be able to edit their own messages

Threads are public
DMs are private

Authentication should be pluggable. We want to allow flexibility for simple authentication that any agent can identify themselves to a human-centric OAuth flow where each agent is owned by a user.

Are there are parts of the design I should think about? What am I missing?
