document.addEventListener('DOMContentLoaded', () => {
    const chatForm = document.getElementById('chat-form');
    const promptInput = document.getElementById('prompt-input');
    const sendBtn = document.getElementById('send-btn');
    const chatHistory = document.getElementById('chat-history');

    // Auto-resize textarea
    promptInput.addEventListener('input', function() {
        this.style.height = 'auto';
        this.style.height = (this.scrollHeight) + 'px';
        
        // Enable/disable send button based on input
        if (this.value.trim().length > 0) {
            sendBtn.removeAttribute('disabled');
        } else {
            sendBtn.setAttribute('disabled', 'true');
        }
    });

    // Handle Enter key (Shift+Enter for new line)
    promptInput.addEventListener('keydown', function(e) {
        if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault();
            if (this.value.trim().length > 0) {
                chatForm.dispatchEvent(new Event('submit'));
            }
        }
    });

    chatForm.addEventListener('submit', async (e) => {
        e.preventDefault();
        
        const message = promptInput.value.trim();
        if (!message) return;

        // Reset input
        promptInput.value = '';
        promptInput.style.height = 'auto';
        sendBtn.setAttribute('disabled', 'true');

        // Append User Message
        appendUserMessage(message);

        // Show typing indicator
        const typingId = showTypingIndicator();

        try {
            const response = await fetch('http://localhost:8081/v1/chat/completions', {
                method: 'POST',
                headers: {
                    'Content-Type': 'application/json',
                    'X-Gateway-API-Key': 'key1',
                    'X-Gateway-Team': 'engineering'
                },
                body: JSON.stringify({
                    messages: [{ role: 'user', content: message }]
                })
            });

            // Remove typing indicator
            document.getElementById(typingId).remove();

            if (!response.ok) {
                throw new Error(`Gateway returned status: ${response.status}`);
            }

            // Extract custom headers
            const cacheStatus = response.headers.get('X-Gateway-Cache') || 'unknown';
            const modelName = response.headers.get('X-Gateway-Model') || 'unknown';

            // Parse response body
            const data = await response.json();
            const aiMessage = data.choices[0].message.content;

            // Append AI Message with Badges
            appendAIMessage(aiMessage, cacheStatus, modelName);

        } catch (error) {
            document.getElementById(typingId)?.remove();
            appendError(`Error: ${error.message}`);
        }
    });

    function appendUserMessage(text) {
        const msgDiv = document.createElement('div');
        msgDiv.className = 'message user slide-in';
        msgDiv.innerHTML = `
            <div class="message-avatar">USR</div>
            <div class="message-content-wrapper">
                <div class="message-content">${escapeHTML(text)}</div>
            </div>
        `;
        chatHistory.appendChild(msgDiv);
        scrollToBottom();
    }

    function appendAIMessage(text, cacheStatus, modelName) {
        // Map cache status to beautiful badge styles
        let badgeHtml = '';
        if (cacheStatus === 'miss') {
            badgeHtml = `<span class="badge badge-cache-miss">Routed to LLM</span>`;
        } else if (cacheStatus === 'redis_hit') {
            badgeHtml = `<span class="badge badge-cache-redis">Redis Cache Hit</span>`;
        } else if (cacheStatus === 'semantic_hit') {
            badgeHtml = `<span class="badge badge-cache-semantic">Semantic Cache Hit</span>`;
        } else {
            badgeHtml = `<span class="badge badge-outline">Status: ${cacheStatus}</span>`;
        }

        // Add Model badge
        const modelBadgeHtml = `<span class="badge badge-model">${escapeHTML(modelName)}</span>`;

        const msgDiv = document.createElement('div');
        msgDiv.className = 'message ai slide-in';
        msgDiv.innerHTML = `
            <div class="message-avatar">AI</div>
            <div class="message-content-wrapper">
                <div class="message-content">${marked.parse(text)}</div>
                <div class="message-meta">
                    ${badgeHtml}
                    ${modelBadgeHtml}
                </div>
            </div>
        `;
        chatHistory.appendChild(msgDiv);
        scrollToBottom();
    }

    function appendError(text) {
        const msgDiv = document.createElement('div');
        msgDiv.className = 'message system-message slide-in';
        msgDiv.innerHTML = `
            <div class="message-avatar" style="color: var(--status-error)">ERR</div>
            <div class="message-content" style="border-color: var(--status-error); color: var(--status-error)">
                <p>${escapeHTML(text)}</p>
                <p style="font-size: 0.8em; margin-top: 10px;">Check if the LLMProxy server is running on localhost:8081.</p>
            </div>
        `;
        chatHistory.appendChild(msgDiv);
        scrollToBottom();
    }

    function showTypingIndicator() {
        const id = 'typing-' + Date.now();
        const msgDiv = document.createElement('div');
        msgDiv.id = id;
        msgDiv.className = 'message ai slide-in';
        msgDiv.innerHTML = `
            <div class="message-avatar">AI</div>
            <div class="message-content-wrapper">
                <div class="message-content">
                    <div class="typing-indicator">
                        <div class="typing-dot"></div>
                        <div class="typing-dot"></div>
                        <div class="typing-dot"></div>
                    </div>
                </div>
            </div>
        `;
        chatHistory.appendChild(msgDiv);
        scrollToBottom();
        return id;
    }

    function scrollToBottom() {
        chatHistory.scrollTop = chatHistory.scrollHeight;
    }

    function escapeHTML(str) {
        return str.replace(/[&<>'"]/g, 
            tag => ({
                '&': '&amp;',
                '<': '&lt;',
                '>': '&gt;',
                "'": '&#39;',
                '"': '&quot;'
            }[tag])
        );
    }
});
